package tgbot

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	stepToken    = "token"
	stepPair     = "pair"
	pairMaxAge   = 10 * time.Minute
	pairMaxWrong = 5
	pairPollSec  = 2
)

var botTokenRE = regexp.MustCompile(`^\d+:[A-Za-z0-9_-]{20,}$`)

type pendingPair struct {
	mu       sync.Mutex
	token    string
	proxy    string
	bot      User
	client   *Client
	code     string
	offset   int64
	started  time.Time
	attempts int
}

// LoginStep handles the two-step BotFather token + 6-digit private-DM pairing flow.
func (b *Bot) LoginStep(ctx context.Context, step string, values map[string]string) (wire.ConnectorPrompt, error) {
	if err := b.load(ctx); err != nil {
		return wire.ConnectorPrompt{}, err
	}
	if step == "" || step == stepToken {
		if step == "" {
			b.clearPair()
			return tokenPrompt(b.lang(), ""), nil
		}
		return b.submitToken(ctx, values)
	}
	if step == stepPair {
		return b.pollPair(ctx)
	}
	return tokenPrompt(b.lang(), ""), nil
}

func (b *Bot) clearPair() {
	b.mu.Lock()
	had := b.pair != nil
	b.pair = nil
	b.mu.Unlock()
	if had {
		b.kick()
	}
}

func tokenPrompt(l, errMsg string) wire.ConnectorPrompt {
	return wire.ConnectorPrompt{
		Step: stepToken,
		Fields: []wire.ConnectorField{
			{
				Name:        "token",
				Label:       t(l, "login.token_label"),
				Secret:      true,
				Placeholder: t(l, "login.token_placeholder"),
			},
			{
				Name:        "proxy",
				Label:       t(l, "login.proxy_label"),
				Placeholder: t(l, "login.proxy_placeholder"),
			},
		},
		Hint:  t(l, "login.token_hint"),
		Error: errMsg,
	}
}

func (p *pendingPair) pairPrompt(l, errMsg string) wire.ConnectorPrompt {
	botURL := "https://telegram.org"
	if p.bot.Username != "" {
		botURL = "https://t.me/" + p.bot.Username
	}
	return wire.ConnectorPrompt{
		Step:     stepPair,
		Kind:     wire.ConnectorPromptKindDeviceCode,
		URL:      botURL,
		UserCode: p.code,
		Interval: pairPollSec,
		Hint:     t(l, "login.pair_hint", "bot", "@"+p.bot.Username, "code", p.code),
		Error:    errMsg,
	}
}

func (b *Bot) submitToken(ctx context.Context, values map[string]string) (wire.ConnectorPrompt, error) {
	l := b.lang()
	token := strings.TrimSpace(values["token"])
	proxy := strings.TrimSpace(values["proxy"])
	if !botTokenRE.MatchString(token) {
		return tokenPrompt(l, t(l, "login.bad_token")), nil
	}
	if _, err := ParseProxyURL(proxy); err != nil {
		return tokenPrompt(l, t(l, "login.bad_proxy")), nil
	}
	c, err := b.opt.NewClient(token, proxy)
	if err != nil {
		return tokenPrompt(l, err.Error()), nil
	}
	me, err := c.GetMe(ctx)
	if err != nil {
		return tokenPrompt(l, t(l, "login.verify_failed", "error", err.Error())), nil
	}
	// Drain stale queued updates so old messages are not confused with a fresh pairing attempt.
	var offset int64
	if old, err := c.GetUpdates(ctx, -1, 0); err == nil && len(old) > 0 {
		offset = old[len(old)-1].UpdateID + 1
	}
	code, err := randomSixDigits()
	if err != nil {
		return wire.ConnectorPrompt{}, err
	}
	p := &pendingPair{
		token:   token,
		proxy:   proxy,
		bot:     me,
		client:  c,
		code:    code,
		offset:  offset,
		started: b.opt.Now(),
	}
	b.mu.Lock()
	b.pair = p
	b.cancelPollLocked()
	b.mu.Unlock()
	b.kick()
	return p.pairPrompt(l, ""), nil
}

func (b *Bot) pollPair(ctx context.Context) (wire.ConnectorPrompt, error) {
	l := b.lang()
	b.mu.Lock()
	p := b.pair
	b.mu.Unlock()
	if p == nil {
		return tokenPrompt(l, ""), nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	b.mu.Lock()
	still := b.pair == p
	b.mu.Unlock()
	if !still {
		return tokenPrompt(l, ""), nil
	}
	if b.opt.Now().Sub(p.started) > pairMaxAge {
		b.clearPair()
		return tokenPrompt(l, t(l, "login.pair_expired")), nil
	}

	updates, err := p.client.GetUpdates(ctx, p.offset, 2)
	if err != nil {
		return p.pairPrompt(l, ""), nil
	}
	for _, upd := range updates {
		if upd.UpdateID+1 > p.offset {
			p.offset = upd.UpdateID + 1
		}
		m := upd.Message
		if m == nil || m.From == nil || m.Chat.Type != "private" {
			continue
		}
		text := strings.TrimSpace(m.Text)
		if text == p.code {
			creds := &credentials{
				Token:       p.token,
				Proxy:       p.proxy,
				BotID:       p.bot.ID,
				BotUsername: p.bot.Username,
				OwnerID:     m.From.ID,
				OwnerName:   m.From.DisplayName(),
				ChatID:      m.Chat.ID,
			}
			b.stopTyping()
			b.mu.Lock()
			if b.pair != p {
				b.mu.Unlock()
				return tokenPrompt(l, ""), nil
			}
			b.sessionGen++
			b.cancelPollLocked()
			b.st = state{
				Creds:  creds,
				Offset: p.offset,
			}
			b.client = p.client
			b.pair = nil
			b.sendGen++
			b.replied = map[int64]time.Time{}
			b.saveLocked(ctx)
			b.mu.Unlock()

			_, _ = p.client.SendMessage(ctx, m.Chat.ID, t(l, "login.paired_reply"), m.MessageID)
			b.setStatus(wire.ConnectorStatus{
				State:   wire.ConnectorStateOk,
				Account: accountLabel(l, creds),
				Via:     t(l, "bot.via"),
			})
			b.kick()
			return wire.ConnectorPrompt{
				Done:    true,
				Account: accountLabel(l, creds),
			}, nil
		}
		p.attempts++
		if p.attempts >= pairMaxWrong {
			b.clearPair()
			return tokenPrompt(l, t(l, "login.too_many_wrong")), nil
		}
		_, _ = p.client.SendMessage(ctx, m.Chat.ID, t(l, "login.wrong_code_reply"), m.MessageID)
	}
	return p.pairPrompt(l, ""), nil
}

func randomSixDigits() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
