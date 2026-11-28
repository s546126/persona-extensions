package wechat

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wechat/internal/wechat/ilink"
	"github.com/nwverse/persona/sdk/go/wire"
)

// Binding is the connector's sign-in: a QR code to scan with WeChat (wire.ConnectorPromptKindQr, asked again every
// interval), WeChat's confirmation on the phone, sometimes a number WeChat shows to type in here.

// pendingQR is the authorization under way; only in memory (a restart starts over with a new code).
type pendingQR struct {
	mu      sync.Mutex
	qr      ilink.QR
	base    string // where to poll: iLink may redirect a scanned code to another origin
	started time.Time
	renewed int
	scanned bool
}

func (p *pendingQR) snapshot() (base string, started time.Time, renewed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.base, p.started, p.renewed
}

func (p *pendingQR) setScanned() {
	p.mu.Lock()
	p.scanned = true
	p.mu.Unlock()
}

func (p *pendingQR) setBase(base string) {
	p.mu.Lock()
	p.base = base
	p.mu.Unlock()
}

const (
	qrInterval = 2
	// qrWait is how long one ask waits on iLink's long poll: short, the app asks again every qrInterval seconds.
	qrWait   = 8 * time.Second
	qrMaxAge = 5 * time.Minute
	stepQR   = "qr"
	stepCode = "code"
)

var verifyCode = regexp.MustCompile(`^\d{1,12}$`)

// authClient is an iLink client for authorizing (no token).
func (b *Bot) authClient() (*ilink.Client, error) { return b.opt.NewClient(ilink.AuthOrigin, "") }

func (b *Bot) LoginStep(ctx context.Context, step string, values map[string]string) (wire.ConnectorPrompt, error) {
	if err := b.load(ctx); err != nil {
		return wire.ConnectorPrompt{}, err
	}
	b.mu.Lock()
	p := b.qr
	b.mu.Unlock()
	var base string
	var started time.Time
	var renewed int
	if p != nil {
		base, started, renewed = p.snapshot()
	}
	if step == "" || p == nil || b.opt.Now().Sub(started) > qrMaxAge {
		return b.beginQR(ctx)
	}
	code := ""
	if step == stepCode {
		code = strings.TrimSpace(values["code"])
		if !verifyCode.MatchString(code) {
			return codePrompt(b.lang(), t(b.lang(), "login.enter_number")), nil
		}
	}
	c, err := b.authClient()
	if err != nil {
		return wire.ConnectorPrompt{}, err
	}
	st, err := c.PollQR(ctx, p.qr.Code, code, base, qrWait)
	if err != nil {
		if ilink.Retryable(err) {
			return p.prompt(b.lang()), nil // asked again in a moment
		}
		log.Printf("wechat: login poll failed: %v", err)
		b.forgetQR(p) // this code cannot succeed any more: the next step starts over with a new one
		return wire.ConnectorPrompt{}, err
	}
	switch st.State {
	case ilink.QRWaiting:
		return p.prompt(b.lang()), nil
	case ilink.QRScanned:
		p.setScanned()
		return p.prompt(b.lang()), nil
	case ilink.QRRedirect:
		p.setBase(st.RedirectBase)
		return p.prompt(b.lang()), nil
	case ilink.QRNeedCode:
		if step == stepCode {
			return codePrompt(b.lang(), t(b.lang(), "login.wrong_number")), nil
		}
		return codePrompt(b.lang(), ""), nil
	case ilink.QRExpired:
		if renewed >= 3 {
			b.forgetQR(p)
			return wire.ConnectorPrompt{}, errors.New(t(b.lang(), "login.qr_expired_too_often"))
		}
		return b.renewQR(ctx, p)
	case ilink.QRCodeBlocked:
		b.forgetQR(p)
		return wire.ConnectorPrompt{}, errors.New(t(b.lang(), "login.code_blocked"))
	case ilink.QRBoundAlready:
		b.forgetQR(p)
		return wire.ConnectorPrompt{}, errors.New(t(b.lang(), "login.bound_already"))
	case ilink.QRConfirmed:
		b.forgetQR(p)
		return b.bound(ctx, st.Credentials), nil
	}
	return p.prompt(b.lang()), nil
}

func (p *pendingQR) prompt(l string) wire.ConnectorPrompt {
	p.mu.Lock()
	scanned := p.scanned
	p.mu.Unlock()
	hint := t(l, "login.scan_hint")
	if scanned {
		hint = t(l, "login.scanned_hint")
	}
	return wire.ConnectorPrompt{Step: stepQR, Kind: wire.ConnectorPromptKindQr, URL: p.qr.Content, Interval: qrInterval, Hint: hint}
}

func codePrompt(l string, errText string) wire.ConnectorPrompt {
	return wire.ConnectorPrompt{Step: stepCode, Fields: []wire.ConnectorField{{Name: "code", Label: t(l, "login.code_label"), Placeholder: t(l, "login.code_placeholder")}},
		Hint: t(l, "login.code_hint"), Error: errText}
}

func (b *Bot) beginQR(ctx context.Context) (wire.ConnectorPrompt, error) {
	return b.beginQRWith(ctx, 0, b.opt.Now())
}

func (b *Bot) beginQRWith(ctx context.Context, renewed int, started time.Time) (wire.ConnectorPrompt, error) {
	c, err := b.authClient()
	if err != nil {
		return wire.ConnectorPrompt{}, err
	}
	b.mu.Lock()
	tokens := append([]string(nil), b.st.Old...)
	if b.st.Creds != nil {
		tokens = rememberToken(tokens, b.st.Creds.Token)
	}
	b.mu.Unlock()
	q, err := c.BeginQR(ctx, tokens)
	if err != nil {
		return wire.ConnectorPrompt{}, err
	}
	p := &pendingQR{qr: q, started: started, renewed: renewed}
	b.mu.Lock()
	b.qr = p
	b.mu.Unlock()
	return p.prompt(b.lang()), nil
}

func (b *Bot) renewQR(ctx context.Context, old *pendingQR) (wire.ConnectorPrompt, error) {
	_, oldStarted, oldRenewed := old.snapshot()
	return b.beginQRWith(ctx, oldRenewed+1, oldStarted)
}

func (b *Bot) forgetQR(p *pendingQR) {
	b.mu.Lock()
	if b.qr == p {
		b.qr = nil
	}
	b.mu.Unlock()
}

// bound keeps the new credentials and starts serving them. The same bot (bound again) keeps its cursor; another
// starts fresh.
func (b *Bot) bound(ctx context.Context, creds ilink.Credentials) wire.ConnectorPrompt {
	b.mu.Lock()
	accountChanged := false
	if old := b.st.Creds; old != nil {
		b.st.Old = rememberToken(b.st.Old, old.Token)
		if old.BotID != creds.BotID || old.UserID != creds.UserID {
			b.st.Cursor, b.st.Context = "", ""
			b.forgetSendsLocked()
			accountChanged = true
		}
	}
	b.st.Creds = &creds
	b.cancelPollLocked()
	b.saveLocked(ctx)
	b.status = wire.ConnectorStatus{State: wire.ConnectorStateOk, Account: account(b.lang(), &creds), Via: t(b.lang(), "wechat.via"), LastEvent: b.status.LastEvent}
	b.mu.Unlock()
	if accountChanged && b.quotes != nil {
		b.quotes.reset()
	}
	b.kick()
	return wire.ConnectorPrompt{Done: true, Account: account(b.lang(), &creds)}
}

// Logout tells iLink the bot is off line and forgets the token (kept among the old ones, so binding again is known
// to be this bot).
func (b *Bot) Logout(ctx context.Context) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	c := b.client
	b.qr = nil
	b.cancelPollLocked()
	b.mu.Unlock()
	if c != nil {
		stop, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = c.NotifyStop(stop)
		cancel()
	}
	b.unbind(ctx, "")
	b.setStatus(wire.ConnectorStatus{State: wire.ConnectorStateNotSignedIn, Error: t(b.lang(), "wechat.not_bound")})
	b.kick()
	return nil
}
