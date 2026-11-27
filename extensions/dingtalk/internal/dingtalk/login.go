package dingtalk

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

// Binding is the connector's sign-in, two steps: the app's Client ID and Client Secret (checked by fetching an access
// token), then a pairing code the app shows (a device_code step, asked again every pairInterval) that the owner sends
// the robot in a one-to-one chat. Whoever sends it is the owner; new credentials always pair again.

const (
	stepCreds    = "creds"
	stepPair     = "pair"
	pairInterval = 3
	pairFor      = 10 * time.Minute
	pairTries    = 5
)

// pairing is the pairing under way; only in memory (a restart starts over with a new code).
type pairing struct {
	code    string
	expires time.Time
	wrong   int
}

func randomCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

func (b *Bot) LoginStep(ctx context.Context, step string, values map[string]string) (wire.ConnectorPrompt, error) {
	if err := b.load(ctx); err != nil {
		return wire.ConnectorPrompt{}, err
	}
	l := b.lang()
	switch step {
	case stepCreds:
		id, secret := strings.TrimSpace(values["client_id"]), strings.TrimSpace(values["client_secret"])
		if id == "" || secret == "" {
			return credsPrompt(l, t(l, "login.creds_missing")), nil
		}
		c, err := b.opt.NewClient(id, secret)
		if err == nil {
			_, err = c.Token(ctx)
		}
		if err != nil {
			return credsPrompt(l, t(l, "login.creds_refused", "error", err.Error())), nil
		}
		b.mu.Lock()
		if b.st.ClientID != id {
			b.st.Seen = nil
		}
		b.st.ClientID, b.st.ClientSecret, b.st.Owner = id, secret, nil
		b.unreadable = false // binding again replaces a stored state that did not read
		b.problem = ""
		b.pairing = &pairing{code: b.opt.Code(), expires: b.opt.Now().Add(pairFor)}
		b.saveLocked(ctx)
		b.reconnectLocked()
		p := b.pairing.prompt(l)
		b.mu.Unlock()
		return p, nil
	case stepPair:
		b.mu.Lock()
		defer b.mu.Unlock()
		if o := b.st.Owner; o != nil && b.pairing == nil {
			return wire.ConnectorPrompt{Done: true, Account: account(l, o)}, nil
		}
		if b.pairing == nil || !b.opt.Now().Before(b.pairing.expires) {
			b.pairing = nil
			return wire.ConnectorPrompt{}, errors.New(t(l, "login.pair_expired"))
		}
		return b.pairing.prompt(l), nil
	}
	return credsPrompt(l, ""), nil
}

func credsPrompt(l, errText string) wire.ConnectorPrompt {
	return wire.ConnectorPrompt{Step: stepCreds, Hint: t(l, "login.creds_hint"), Error: errText, Fields: []wire.ConnectorField{
		{Name: "client_id", Label: t(l, "login.client_id_label"), Placeholder: t(l, "login.client_id_placeholder")},
		{Name: "client_secret", Label: t(l, "login.client_secret_label"), Secret: true},
	}}
}

func (p *pairing) prompt(l string) wire.ConnectorPrompt {
	return wire.ConnectorPrompt{Step: stepPair, Kind: wire.ConnectorPromptKindDeviceCode, UserCode: p.code,
		Interval: pairInterval, Hint: t(l, "login.pair_hint")}
}

// tryPairLocked takes a message from someone who is not the owner while a pairing is under way: the code makes them
// the owner. It answers what to tell them, and whether the message was about pairing at all; b.mu is held.
func (b *Bot) tryPairLocked(ctx context.Context, staffID, name, text string) (reply string, handled bool) {
	l := b.lang()
	p := b.pairing
	if p == nil {
		return "", false
	}
	if !b.opt.Now().Before(p.expires) {
		b.pairing = nil
		return "", false
	}
	text = strings.TrimSpace(text)
	if !looksLikeCode(text) {
		return "", false
	}
	if text != p.code {
		p.wrong++
		if p.wrong >= pairTries {
			b.pairing = nil
			return t(l, "chat.code_void"), true
		}
		return t(l, "chat.wrong_code"), true
	}
	b.pairing = nil
	b.st.Owner = &owner{StaffID: staffID, Name: name, PairedAt: b.opt.Now()}
	b.saveLocked(ctx)
	return t(l, "chat.paired"), true
}

// looksLikeCode says text is a pairing attempt: six digits.
func looksLikeCode(text string) bool {
	if len(text) != 6 {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Logout forgets the app's credentials and the owner, and ends the connection.
func (b *Bot) Logout(ctx context.Context) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st, b.pairing, b.problem, b.live, b.unreadable = state{}, nil, "", false, false
	b.reconnectLocked()
	return b.secrets.Delete(ctx)
}
