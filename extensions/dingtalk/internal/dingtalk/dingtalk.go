// Package dingtalk is a DingTalk robot for Persona: the robot of the user's own DingTalk internal app, reached over
// Stream mode (an outbound WebSocket, so the hub needs no public address). The extension contributes a channel and
// a connector whose sign-in takes the app's credentials and then a pairing code: only the person who sent that code
// to the robot reaches the hub. Conversation routing and turn handling remain the hub's. The credentials, the owner
// and the deduplication state live in the hub's sealed connector secret.
package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/nwverse/persona-extensions/extensions/dingtalk/internal/dingtalk/api"
	"github.com/nwverse/persona/sdk/go/wire"
)

// ID is the extension's, its connector's and its channel's; Name is how DingTalk calls itself, as the hub registers it.
const (
	ID   = "dingtalk"
	Name = "钉钉" // i18n-ignore: the stable localized connector name
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

// errCannotSend: there is nowhere to send to yet (not set up, or nobody paired); the hub hears cannot_send.
var errCannotSend = errors.New("nowhere to send to yet")

// Options are the robot's; NewClient, Now and Code are what tests replace.
type Options struct {
	// Hub takes the owner's messages (the extension protocol's channel namespace).
	Hub   Hub
	Agent string // the product token DingTalk sees ("Persona/<version>")
	// Lang is the user's language as the hub said it at this start (en, zh-Hans, ja): what the robot writes for
	// people reads in it (zh-Hans when not said).
	Lang string
	// NewClient makes the DingTalk client for an app's credentials (api.New when nil).
	NewClient func(clientID, clientSecret string) (*api.Client, error)
	Now       func() time.Time
	// Code makes a pairing code (six random digits when nil).
	Code func() string
}

// Bot is the connector and the robot behind it.
type Bot struct {
	opt     Options
	secrets Secrets

	mu      sync.Mutex
	st      state
	loaded  bool
	client  *api.Client // for the saved credentials; nil while none
	live    bool        // the Stream connection is up
	problem string      // why it is not, for people ("" when nothing is wrong)
	last    time.Time   // the owner's latest message
	pairing *pairing
	wake    chan struct{}
	restart context.CancelFunc // ends the connection of the credentials it serves (they changed)
	replied map[string]time.Time
	queue   chan queued
	// hubErrorTold are the owner's messages the hub failed to take that the owner was already told about.
	hubErrorTold map[string]bool
	// unreadable: the sealed state did not read; nothing is saved over it until the user binds again or signs out.
	unreadable bool
}

// state is what is kept in the hub (sealed): never logged.
type state struct {
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	Owner        *owner `json:"owner,omitempty"`
	// Seen are the ids of the latest messages handled (DingTalk may deliver one again).
	Seen []string `json:"seen,omitempty"`
}

// owner is the person who paired: the only one whose messages reach the hub.
type owner struct {
	StaffID  string    `json:"staff_id"`
	Name     string    `json:"name,omitempty"`
	PairedAt time.Time `json:"paired_at"`
}

// queued is a message the Stream took, waiting for its turn.
type queued struct {
	c    *api.Client
	m    api.Message
	done func() // answers its Stream frame
}

// queueMost is how many taken messages wait at once; more are left unanswered, so DingTalk delivers them again.
const queueMost = 64

func New(opt Options, secrets Secrets) *Bot {
	if opt.NewClient == nil {
		opt.NewClient = func(id, secret string) (*api.Client, error) {
			return api.New(api.Options{ClientID: id, ClientSecret: secret, Agent: opt.Agent})
		}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Code == nil {
		opt.Code = randomCode
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
		s.State, s.Error = wire.ConnectorStateError, t(l, "dingtalk.cannot_load", "error", t(l, "dingtalk.unreadable"))
	case b.st.ClientID == "":
		s.State, s.Error = wire.ConnectorStateNotSignedIn, t(l, "dingtalk.not_configured")
		s.Guide = []string{t(l, "guide.create_app"), t(l, "guide.robot"), t(l, "guide.bind")}
	case b.st.Owner == nil:
		s.State, s.Error = wire.ConnectorStateNotSignedIn, t(l, "dingtalk.not_paired")
	default:
		s.Account, s.Via = account(l, b.st.Owner), t(l, "dingtalk.via")
		switch {
		case b.problem != "":
			s.State, s.Error = wire.ConnectorStateError, b.problem
		case !b.live:
			s.State, s.Error = wire.ConnectorStateError, t(l, "dingtalk.connecting")
		default:
			s.State = wire.ConnectorStateOk
		}
	}
	return s
}

// account is how the paired person is shown: their DingTalk name.
func account(l string, o *owner) string {
	if o.Name != "" {
		return o.Name
	}
	return t(l, "dingtalk.account_unnamed")
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
			log.Printf("dingtalk: the stored state does not read; keeping it until bound again: %v", err)
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
		log.Printf("dingtalk: saving the state: %v", err)
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

// Run holds the Stream connection while the app's credentials are set, and hands what comes to the hub.
func (b *Bot) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := b.load(ctx)
		if err == nil {
			break
		}
		b.mu.Lock()
		b.problem = t(b.lang(), "dingtalk.cannot_load", "error", err.Error())
		b.mu.Unlock()
		if !sleep(ctx, 5*time.Second) {
			return
		}
	}
	go b.work(ctx)
	for ctx.Err() == nil {
		b.mu.Lock()
		id, secret := b.st.ClientID, b.st.ClientSecret
		b.mu.Unlock()
		if id == "" {
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

// serve keeps a Stream connection for the credentials until they change or ctx ends.
func (b *Bot) serve(parent context.Context, id, secret string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	c, err := b.opt.NewClient(id, secret)
	b.mu.Lock()
	if b.st.ClientID != id || b.st.ClientSecret != secret {
		b.mu.Unlock()
		b.kick() // the credentials changed under it: Run starts over with the new ones
		return
	}
	b.restart = cancel
	if err == nil {
		b.client = c
	} else {
		b.problem = t(b.lang(), "dingtalk.refused", "error", err.Error())
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.client == c {
			b.client, b.live = nil, false
		}
		b.mu.Unlock()
	}()
	if err != nil {
		<-ctx.Done() // nothing to do until the credentials change
		return
	}
	failures := 0
	for ctx.Err() == nil {
		err := c.Stream(ctx, []string{api.BotTopic}, func() {
			b.mu.Lock()
			b.live, b.problem = true, ""
			b.mu.Unlock()
			failures = 0
		}, func(_ context.Context, f api.Frame, done func()) { b.take(c, f, done) })
		b.mu.Lock()
		b.live = false
		b.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, api.ErrMoved) {
			continue
		}
		key := "dingtalk.retrying"
		if api.Unauthorized(err) {
			key = "dingtalk.refused"
		}
		b.mu.Lock()
		b.problem = t(b.lang(), key, "error", err.Error())
		b.mu.Unlock()
		wait := backoff[min(failures, len(backoff)-1)]
		failures++
		if !sleep(ctx, wait) {
			return
		}
	}
}

// take queues a message the Stream brought; one that does not fit in the queue is left unanswered, so DingTalk
// delivers it again.
func (b *Bot) take(c *api.Client, f api.Frame, done func()) {
	var m api.Message
	if err := json.Unmarshal([]byte(f.Data), &m); err != nil || m.ID == "" {
		done() // nothing it can read: answered, so it is not delivered again
		return
	}
	select {
	case b.queue <- queued{c: c, m: m, done: done}:
	default:
	}
}

// work handles the taken messages one at a time, in the order they came, answering each frame once it is settled.
// Messages still queued when it stops are never answered, and DingTalk delivers them again.
func (b *Bot) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-b.queue:
			if b.handle(ctx, q.c, q.m) {
				q.done()
			}
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
