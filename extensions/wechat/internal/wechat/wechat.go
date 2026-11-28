// Package wechat implements a personal WeChat bot over the public iLink protocol.
// The extension contributes a channel and a QR-login connector. Only the bound user's
// messages reach the hub; conversation routing and turn handling remain the hub's.
// It uses the official Go extension SDK and keeps the existing token, polling cursor,
// context token and deduplication state in the hub's sealed connector secret.
package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wechat/internal/wechat/ilink"
	"github.com/nwverse/persona/sdk/go/wire"
)

// ID is the plugin's, its connector's and its channel's; Name is how WeChat calls itself, as the hub registers it.
const (
	ID   = "wechat"
	Name = "微信" // i18n-ignore: the stable localized connector name
)

// Hub is the hub as the bot hands it the owner's messages (channel.receive).
type Hub interface {
	Receive(ctx context.Context, m wire.ChannelMessage) error
}

// Secrets is the connector's sealed blob in the hub (connector.secrets.*): nil from Load when nothing is kept.
type Secrets interface {
	Load(ctx context.Context) ([]byte, error)
	Save(ctx context.Context, data []byte) error
	Delete(ctx context.Context) error
}

// errCannotSend: there is nowhere to send to yet (before the owner's first message); the hub hears cannot_send.
var errCannotSend = errors.New("nowhere to send to yet")

// errRebound: the WeChat a channel.send came in for is no longer bound (the send waited for an earlier send of its ref
// while the binding changed); the hub hears cannot_send, so it does not send the message again to the WeChat bound now.
var errRebound = errors.New("the WeChat it was for is no longer bound")

// Clock and iLink are what tests replace.
type Options struct {
	// Hub takes the owner's messages (the plugin protocol's channel namespace).
	Hub   Hub
	Agent string // the product token iLink sees ("Persona/<version>")
	// Lang is the user's language as the hub said it at this start (en, zh-Hans, ja): what the bot writes for people
	// reads in it (zh-Hans when not said).
	Lang string
	// DataDir is the extension-owned local directory for the quote index (empty means memory-only).
	DataDir string
	// NewClient makes the iLink client for a base URL and token (ilink.New when nil).
	NewClient func(base, token string) (*ilink.Client, error)
	Now       func() time.Time
}

// Bot is the connector and the bot behind it.
type Bot struct {
	opt     Options
	secrets Secrets

	mu         sync.Mutex
	st         state
	loaded     bool
	status     wire.ConnectorStatus
	client     *ilink.Client // for the bound token; nil while not bound
	pollCancel context.CancelFunc
	qr         *pendingQR
	wake       chan struct{}
	typing     *typing // the owner's chat shows the bot writing (the hub says while a turn goes on)
	replied    map[string]time.Time
	quotes     *quoteStore
	// progress is how far each ref's channel.send got, oldest ref first in progressOrder (chat.go sendProgress).
	progress      map[string]sendProgress
	progressOrder []string
	// sendGen counts binding changes that dropped the progress (forgetSendsLocked): a send keeps or reads progress
	// only in the generation it started in.
	sendGen uint64
	// rounds are the latest round of each ref's send, oldest ref first in roundOrder (chat.go roundLocked).
	rounds     map[string]sendRound
	roundOrder []string
}

// state is what is kept in the hub (sealed): never logged.
type state struct {
	Creds *ilink.Credentials `json:"creds,omitempty"`
	// Old are bot tokens it had before (the last 10): a new authorization sends them so iLink knows it is this bot.
	Old    []string `json:"old,omitempty"`
	Cursor string   `json:"cursor,omitempty"`
	// Context is the owner's latest message's context token: iLink needs one to send anything.
	Context string `json:"context,omitempty"`
	// Seen are the ids of the latest messages handled (iLink may deliver one again).
	Seen []string `json:"seen,omitempty"`
}

func New(opt Options, secrets Secrets) *Bot {
	if opt.NewClient == nil {
		opt.NewClient = func(base, token string) (*ilink.Client, error) {
			return ilink.New(ilink.Options{BaseURL: base, Token: token, Agent: opt.Agent})
		}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Bot{opt: opt, secrets: secrets, wake: make(chan struct{}, 1), replied: map[string]time.Time{},
		quotes: openQuoteStore(opt.DataDir, opt.Now),
		status: wire.ConnectorStatus{State: wire.ConnectorStateNotSignedIn, Error: t(langOf(opt.Lang), "wechat.not_bound")}}
}

// lang is the language of what the bot writes for people.
func (b *Bot) lang() string { return langOf(b.opt.Lang) }

// Status is answered from memory.
func (b *Bot) Status(context.Context) wire.ConnectorStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *Bot) setStatus(s wire.ConnectorStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s.LastEvent = b.status.LastEvent
	b.status = s
}

// account is how the bound WeChat is shown, in l: iLink gives an id, not a name.
func account(l string, c *ilink.Credentials) string {
	id, _, _ := strings.Cut(c.UserID, "@")
	if r := []rune(id); len(r) > 6 {
		id = string(r[len(r)-6:])
	}
	return t(l, "wechat.account", "id", id)
}

func (b *Bot) load(ctx context.Context) error {
	b.mu.Lock()
	if b.loaded {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()
	data, err := b.secrets.Load(ctx)
	if err != nil {
		return err
	}
	var st state
	if len(data) > 0 {
		if err := json.Unmarshal(data, &st); err != nil {
			log.Printf("wechat: the stored state does not read, starting over: %v", err)
			st = state{}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.loaded {
		b.st, b.loaded = st, true
	}
	return nil
}

// saveLocked stores the state; b.mu is held.
func (b *Bot) saveLocked(ctx context.Context) {
	data, err := json.Marshal(b.st)
	if err == nil {
		err = b.secrets.Save(ctx, data)
	}
	if err != nil {
		log.Printf("wechat: saving the state: %v", err)
	}
}

func (b *Bot) kick() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Run polls iLink for the owner's messages while bound.
func (b *Bot) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := b.load(ctx); err == nil {
			break
		} else {
			b.setStatus(wire.ConnectorStatus{State: wire.ConnectorStateError, Error: t(b.lang(), "wechat.cannot_load", "error", err.Error())})
		}
		if !sleep(ctx, 5*time.Second) {
			return
		}
	}
	for ctx.Err() == nil {
		b.mu.Lock()
		creds := b.st.Creds
		b.mu.Unlock()
		if creds == nil {
			if b.Status(ctx).State != wire.ConnectorStateNotSignedIn { // keep why it was unbound
				b.setStatus(wire.ConnectorStatus{State: wire.ConnectorStateNotSignedIn, Error: t(b.lang(), "wechat.not_bound")})
			}
			select {
			case <-ctx.Done():
				return
			case <-b.wake:
			}
			continue
		}
		b.serve(ctx, *creds)
	}
}

var backoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}

func (b *Bot) cancelPollLocked() {
	if b.pollCancel != nil {
		b.pollCancel()
		b.pollCancel = nil
	}
}

func (b *Bot) isBound(token string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st.Creds != nil && b.st.Creds.Token == token
}

// serve long-polls with creds until they change, are refused or ctx ends.
func (b *Bot) serve(ctx context.Context, creds ilink.Credentials) {
	c, err := b.opt.NewClient(creds.BaseURL, creds.Token)
	if err != nil {
		b.setStatus(wire.ConnectorStatus{State: wire.ConnectorStateError, Account: account(b.lang(), &creds), Error: t(b.lang(), "wechat.bad_address", "error", err.Error())})
		b.unbind(ctx, "")
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	if b.st.Creds == nil || b.st.Creds.Token != creds.Token {
		b.mu.Unlock()
		cancel()
		return
	}
	b.client = c
	b.pollCancel = cancel
	b.mu.Unlock()
	defer func() {
		cancel()
		b.mu.Lock()
		if b.client == c {
			b.client = nil
			b.pollCancel = nil
		}
		b.mu.Unlock()
	}()
	ok := wire.ConnectorStatus{State: wire.ConnectorStateOk, Account: account(b.lang(), &creds), Via: t(b.lang(), "wechat.via")}
	if err := c.NotifyStart(sctx); err != nil && sctx.Err() == nil {
		log.Printf("wechat: notify start: %v", err) // not needed to receive messages
	}
	if !b.isBound(creds.Token) || sctx.Err() != nil {
		return
	}
	b.setStatus(ok)
	failures := 0
	for sctx.Err() == nil {
		b.mu.Lock()
		still := b.st.Creds != nil && b.st.Creds.Token == creds.Token
		cursor := b.st.Cursor
		b.mu.Unlock()
		if !still {
			return
		}
		p, err := c.GetUpdates(sctx, cursor)
		if err != nil {
			if sctx.Err() != nil {
				break
			}
			if !b.isBound(creds.Token) {
				return
			}
			switch {
			case ilink.CodeOf(err) == ilink.CodeReplaced:
				b.unbind(ctx, creds.Token)
				b.setStatus(wire.ConnectorStatus{State: wire.ConnectorStateNotSignedIn, Error: t(b.lang(), "wechat.revoked")})
				return
			case !ilink.Retryable(err):
				b.setStatus(wire.ConnectorStatus{State: wire.ConnectorStateError, Account: ok.Account, Error: t(b.lang(), "wechat.refused", "error", err.Error())})
			default:
				b.setStatus(wire.ConnectorStatus{State: wire.ConnectorStateError, Account: ok.Account, Error: t(b.lang(), "wechat.retrying", "error", err.Error())})
			}
			wait := backoff[min(failures, len(backoff)-1)]
			failures++
			if !sleep(sctx, wait) {
				break
			}
			continue
		}
		if !b.isBound(creds.Token) || sctx.Err() != nil {
			break
		}
		if failures > 0 {
			b.setStatus(ok)
			failures = 0
		}
		for _, m := range p.Messages {
			if !b.isBound(creds.Token) || sctx.Err() != nil {
				break
			}
			b.handle(sctx, c, creds, m)
		}
		b.mu.Lock()
		if b.st.Creds != nil && b.st.Creds.Token == creds.Token && b.st.Cursor != p.Cursor {
			b.st.Cursor = p.Cursor
			b.saveLocked(ctx)
		}
		b.mu.Unlock()
	}
	if ctx.Err() != nil {
		stop, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stopCancel()
		_ = c.NotifyStop(stop)
	}
}

// unbind forgets the bound token (only if it is still token, when given), keeping it among the old ones.
func (b *Bot) unbind(ctx context.Context, token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Creds == nil || token != "" && b.st.Creds.Token != token {
		return
	}
	b.cancelPollLocked()
	b.st.Old = rememberToken(b.st.Old, b.st.Creds.Token)
	b.st.Creds, b.st.Cursor, b.st.Context = nil, "", ""
	b.typing = nil
	b.replied = map[string]time.Time{}
	b.status.LastEvent = time.Time{}
	b.forgetSendsLocked()
	b.saveLocked(ctx)
	if b.quotes != nil {
		b.quotes.reset()
	}
}

func rememberToken(old []string, token string) []string {
	old = slices.DeleteFunc(old, func(t string) bool { return t == token })
	old = append(old, token)
	if len(old) > 10 {
		old = old[len(old)-10:]
	}
	return old
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
