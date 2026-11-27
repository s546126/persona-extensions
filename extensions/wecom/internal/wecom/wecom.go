// Package wecom is a WeCom robot for Persona: a smart robot (智能机器人) of the user's own WeCom organization in API
// mode, reached over its long connection (an outbound WebSocket, so the hub needs no public address). The extension
// contributes a channel and a connector whose sign-in takes the robot's BotID and Secret and then a pairing code:
// only the person who sent that code to the robot reaches the hub. Conversation routing and turn handling remain the
// hub's. The credentials, the owner and the deduplication state live in the hub's sealed connector secret.
package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wecom/internal/wecom/api"
	"github.com/nwverse/persona/sdk/go/wire"
)

// ID is the extension's, its connector's and its channel's; Name is how WeCom calls itself, as the hub registers it.
const (
	ID   = "wecom"
	Name = "企业微信" // i18n-ignore: the stable localized connector name
)

// Hub is the hub as the robot hands it the owner's messages (channel.receive).
type Hub interface {
	Receive(ctx context.Context, m wire.ChannelMessage) error
}

// Secrets is the connector's sealed blob in the hub (connector.secrets.*): nil from Load when nothing is kept.
type Secrets interface {
	Load(ctx context.Context) ([]byte, error)
	Save(ctx context.Context, data []byte) error
	Delete(ctx context.Context) error
}

// errCannotSend: there is nowhere to send to yet (not set up, not connected, or nobody paired); the hub hears
// cannot_send.
var errCannotSend = errors.New("nowhere to send to yet")

// Options are the robot's; NewClient, Now and Code are what tests replace.
type Options struct {
	// Hub takes the owner's messages (the extension protocol's channel namespace).
	Hub Hub
	// Lang is the user's language as the hub said it at this start (en, zh-Hans, ja): what the robot writes for
	// people reads in it (zh-Hans when not said).
	Lang string
	// NewClient makes the WeCom client for a robot's credentials (api.New when nil).
	NewClient func(botID, secret string) (*api.Client, error)
	Now       func() time.Time
	// Code makes a pairing code (six random digits when nil).
	Code func() string
	// KickedWait is how long it waits before taking the robot back after another connection took it over.
	KickedWait time.Duration
}

// Bot is the connector and the robot behind it.
type Bot struct {
	opt     Options
	secrets Secrets

	mu      sync.Mutex
	st      state
	loaded  bool
	client  *api.Client
	session *api.Session // the subscribed connection; nil while there is none
	problem string       // why it is not connected, for people ("" when nothing is wrong)
	last    time.Time    // the owner's latest message
	pairing *pairing
	wake    chan struct{}
	restart context.CancelFunc // ends the connection of the credentials it serves (they changed)
	replied map[string]time.Time
	queue   chan queued
	// paused holds Run back while binding checks new credentials, so the check is the robot's only subscriber.
	paused bool
	// serving is closed when the current serve returns (nil while none runs).
	serving chan struct{}
	// hubErrorTold are the owner's messages the hub failed to take that the owner was already told about.
	hubErrorTold map[string]bool
	// unreadable: the sealed state did not read; nothing is saved over it until the user binds again or signs out.
	unreadable bool
}

// state is what is kept in the hub (sealed): never logged.
type state struct {
	BotID  string `json:"bot_id,omitempty"`
	Secret string `json:"secret,omitempty"`
	Owner  *owner `json:"owner,omitempty"`
	// Seen are the ids of the latest messages handled (WeCom may deliver one again).
	Seen []string `json:"seen,omitempty"`
}

// owner is the person who paired: the only one whose messages reach the hub. UserID is how WeCom names them to this
// robot (plain, or an encrypted id when the robot's creator is not a super admin; stable either way).
type owner struct {
	UserID   string    `json:"user_id"`
	PairedAt time.Time `json:"paired_at"`
}

// queued is a message the connection brought, waiting for its turn.
type queued struct {
	c *api.Client
	m api.Message
}

// queueMost is how many messages wait at once; more are dropped with a log line (WeCom does not deliver again).
const queueMost = 64

func New(opt Options, secrets Secrets) *Bot {
	if opt.NewClient == nil {
		opt.NewClient = func(id, secret string) (*api.Client, error) {
			return api.New(api.Options{BotID: id, Secret: secret})
		}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Code == nil {
		opt.Code = randomCode
	}
	if opt.KickedWait == 0 {
		opt.KickedWait = 30 * time.Minute
	}
	return &Bot{opt: opt, secrets: secrets, wake: make(chan struct{}, 1), replied: map[string]time.Time{},
		queue: make(chan queued, queueMost), hubErrorTold: map[string]bool{}}
}

// lang is the language of what the robot writes for people.
func (b *Bot) lang() string { return langOf(b.opt.Lang) }

// Status is answered from memory.
func (b *Bot) Status(context.Context) wire.ConnectorStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.lang()
	s := wire.ConnectorStatus{LastEvent: b.last}
	switch {
	case !b.loaded && b.problem != "":
		s.State, s.Error = wire.ConnectorStateError, b.problem
	case b.unreadable:
		s.State, s.Error = wire.ConnectorStateError, t(l, "wecom.cannot_load", "error", t(l, "wecom.unreadable"))
	case b.st.BotID == "":
		s.State, s.Error = wire.ConnectorStateNotSignedIn, t(l, "wecom.not_configured")
		s.Guide = []string{t(l, "guide.create_robot"), t(l, "guide.long_connection"), t(l, "guide.bind")}
	case b.st.Owner == nil:
		s.State, s.Error = wire.ConnectorStateNotSignedIn, t(l, "wecom.not_paired")
	default:
		s.Account, s.Via = t(l, "wecom.account"), t(l, "wecom.via")
		switch {
		case b.problem != "":
			s.State, s.Error = wire.ConnectorStateError, b.problem
		case b.session == nil:
			s.State, s.Error = wire.ConnectorStateError, t(l, "wecom.connecting")
		default:
			s.State = wire.ConnectorStateOk
		}
	}
	return s
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
	unreadable := false
	if len(data) > 0 {
		if err := json.Unmarshal(data, &st); err != nil {
			log.Printf("wecom: the stored state does not read; keeping it until bound again: %v", err)
			st, unreadable = state{}, true
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.loaded {
		b.st, b.loaded, b.problem, b.unreadable = st, true, "", unreadable
	}
	return nil
}

// saveLocked stores the state; b.mu is held. A stored state that did not read is never overwritten by accident.
func (b *Bot) saveLocked(ctx context.Context) {
	if b.unreadable {
		return
	}
	data, err := json.Marshal(b.st)
	if err == nil {
		err = b.secrets.Save(ctx, data)
	}
	if err != nil {
		log.Printf("wecom: saving the state: %v", err)
	}
}

func (b *Bot) kick() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// reconnectLocked ends the connection of the credentials it serves, so Run starts over with the current ones;
// b.mu is held.
func (b *Bot) reconnectLocked() {
	if b.restart != nil {
		b.restart()
		b.restart = nil
	}
	b.kick()
}

// Run holds the long connection while the robot's credentials are set, and hands what comes to the hub.
func (b *Bot) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := b.load(ctx)
		if err == nil {
			break
		}
		b.mu.Lock()
		b.problem = t(b.lang(), "wecom.cannot_load", "error", err.Error())
		b.mu.Unlock()
		if !sleep(ctx, 5*time.Second) {
			return
		}
	}
	go b.work(ctx)
	for ctx.Err() == nil {
		b.mu.Lock()
		id, secret, paused := b.st.BotID, b.st.Secret, b.paused
		b.mu.Unlock()
		if id == "" || paused {
			select {
			case <-ctx.Done():
				return
			case <-b.wake:
			}
			continue
		}
		b.serve(ctx, id, secret)
	}
}

var backoff = []time.Duration{time.Second, 3 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute}

// serve keeps a subscribed connection for the credentials until they change or ctx ends.
func (b *Bot) serve(parent context.Context, id, secret string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	c, err := b.opt.NewClient(id, secret)
	b.mu.Lock()
	if b.st.BotID != id || b.st.Secret != secret || b.paused {
		b.mu.Unlock()
		b.kick() // the credentials changed under it: Run starts over with the new ones
		return
	}
	b.serving = done
	b.restart = cancel
	if err == nil {
		b.client = c
	} else {
		b.problem = t(b.lang(), "wecom.refused", "error", err.Error())
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.client == c {
			b.client, b.session = nil, nil
		}
		b.mu.Unlock()
	}()
	if err != nil {
		<-ctx.Done() // nothing to do until the credentials change
		return
	}
	failures := 0
	for ctx.Err() == nil {
		err := c.Run(ctx, func(s *api.Session) {
			b.mu.Lock()
			b.session, b.problem = s, ""
			b.mu.Unlock()
			failures = 0
		}, func(f api.Frame) { b.take(c, f) })
		b.mu.Lock()
		b.session = nil
		b.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		key, wait := "wecom.retrying", backoff[min(failures, len(backoff)-1)]
		switch {
		case errors.Is(err, api.ErrKicked):
			key, wait = "wecom.kicked", b.opt.KickedWait
		case api.Unauthorized(err):
			key = "wecom.refused"
		}
		failures++
		b.mu.Lock()
		b.problem = t(b.lang(), key, "error", api.ErrText(err))
		b.mu.Unlock()
		if !sleep(ctx, wait) {
			return
		}
	}
}

// take queues a message the connection brought; events other than being taken over are not used.
func (b *Bot) take(c *api.Client, f api.Frame) {
	if f.Cmd != api.CmdMessage {
		return
	}
	m, err := api.ParseMessage(f.Body)
	if err != nil || m.ID == "" {
		return
	}
	select {
	case b.queue <- queued{c: c, m: m}:
	default:
		log.Printf("wecom: too many messages waiting; one was dropped")
	}
}

// work handles the queued messages one at a time, in the order they came.
func (b *Bot) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-b.queue:
			b.handle(ctx, q.c, q.m)
		}
	}
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
