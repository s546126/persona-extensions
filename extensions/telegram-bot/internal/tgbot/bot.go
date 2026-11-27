// Package tgbot implements a personal Telegram Bot API channel and connector.
// Only the single owner who pairs with the 6-digit code in a private chat can
// talk to Persona; groups and strangers are ignored or given a rate-limited notice.
package tgbot

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	// ExtID is the extension manifest id; ID is the connector and channel contribution id.
	ExtID   = "telegram-bot"
	ID      = "telegrambot"
	Name    = "Telegram 机器人" // i18n-ignore: stable localized connector name
	MaxText = 4000
)

var (
	// Capabilities are the connector's capabilities: status and interactive login.
	Capabilities = []string{wire.ConnectorCapabilityStatus, wire.ConnectorCapabilityLogin}
	// Network is the host allowlist declared in the manifest.
	Network = []string{"api.telegram.org"}
)

// Item is the connector item declared in the manifest.
func Item() wire.ConnectorItem {
	return wire.ConnectorItem{ID: ID, Name: Name, Capabilities: Capabilities}
}

// Channel is the channel item declared in the manifest.
func Channel() wire.ChannelItem {
	return wire.ChannelItem{
		ID:          ID,
		Name:        Name,
		Kind:        "im",
		Connector:   ID,
		Bind:        "fields",
		Description: "在 Telegram 里私聊自己的 Bot：填 BotFather 令牌并用配对码绑定本人，消息进主对话，回答发回 Telegram", // i18n-ignore: matches manifest
		Capabilities: wire.ChannelCapabilities{
			ImagesIn:  true,
			ImagesOut: true,
			FilesIn:   true,
			Format:    "plain",
			MaxText:   MaxText,
			Asks:      "text",
			Push:      true,
			Typing:    true,
		},
	}
}

// Hub delivers owner messages to Persona via channel.receive.
type Hub interface {
	Receive(ctx context.Context, m wire.ChannelMessage) error
}

// Secrets loads, saves, and deletes the hub-encrypted connector secret.
type Secrets interface {
	Load(ctx context.Context) ([]byte, error)
	Save(ctx context.Context, data []byte) error
	Delete(ctx context.Context) error
}

// Options configures a Bot instance.
type Options struct {
	Hub       Hub
	Lang      string
	BaseURL   string
	NewClient func(token, proxy string) (*Client, error)
	Now       func() time.Time
}

type credentials struct {
	Token       string `json:"token"`
	Proxy       string `json:"proxy,omitempty"`
	BotID       int64  `json:"bot_id,omitempty"`
	BotUsername string `json:"bot_username,omitempty"`
	OwnerID     int64  `json:"owner_id"`
	OwnerName   string `json:"owner_name,omitempty"`
	ChatID      int64  `json:"chat_id"`
}

type state struct {
	Creds  *credentials `json:"creds,omitempty"`
	Offset int64        `json:"offset,omitempty"`
	Seen   []int64      `json:"seen,omitempty"`
}

// Bot implements the Telegram Bot connector and channel.
type Bot struct {
	opt     Options
	secrets Secrets

	mu      sync.Mutex
	st      state
	loaded  bool
	status  wire.ConnectorStatus
	client  *Client
	pair    *pendingPair
	wake    chan struct{}
	typing  *typingLoop
	replied map[int64]time.Time
	sendGen uint64
}

var errCannotSend = errors.New("nowhere to send to yet")

// New constructs a Bot.
func New(opt Options, secrets Secrets) *Bot {
	if opt.BaseURL == "" {
		opt.BaseURL = DefaultBaseURL
	}
	if opt.NewClient == nil {
		base := opt.BaseURL
		opt.NewClient = func(token, proxy string) (*Client, error) {
			return NewClient(base, token, proxy)
		}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Bot{
		opt:     opt,
		secrets: secrets,
		wake:    make(chan struct{}, 1),
		replied: map[int64]time.Time{},
		status:  wire.ConnectorStatus{State: wire.ConnectorStateNotSignedIn, Error: t(langOf(opt.Lang), "bot.not_bound")},
	}
}

func (b *Bot) lang() string { return langOf(b.opt.Lang) }

// Status returns the connector's current status from memory.
func (b *Bot) Status(context.Context) wire.ConnectorStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *Bot) setStatus(s wire.ConnectorStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.LastEvent.IsZero() {
		s.LastEvent = b.status.LastEvent
	}
	b.status = s
}

func (b *Bot) kick() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
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
			log.Printf("telegram-bot: saved state could not be decoded, starting fresh")
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

func (b *Bot) saveLocked(ctx context.Context) {
	data, err := json.Marshal(b.st)
	if err == nil {
		err = b.secrets.Save(ctx, data)
	}
	if err != nil {
		log.Printf("telegram-bot: save secret: %v", err)
	}
}

// Run polls Telegram updates whenever a bound bot token is present.
func (b *Bot) Run(ctx context.Context) {
	if err := b.load(ctx); err != nil {
		b.setStatus(wire.ConnectorStatus{
			State: wire.ConnectorStateError,
			Error: t(b.lang(), "bot.cannot_load", "error", err.Error()),
		})
		return
	}
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		b.mu.Lock()
		creds := b.st.Creds
		offset := b.st.Offset
		pairing := b.pair != nil
		b.mu.Unlock()

		if creds == nil || creds.Token == "" || creds.ChatID == 0 {
			b.setStatus(wire.ConnectorStatus{
				State: wire.ConnectorStateNotSignedIn,
				Error: t(b.lang(), "bot.not_bound"),
			})
			select {
			case <-ctx.Done():
				return
			case <-b.wake:
			}
			continue
		}
		if pairing {
			// Pause background polling while LoginStep("pair") reads updates.
			select {
			case <-ctx.Done():
				return
			case <-b.wake:
			case <-time.After(2 * time.Second):
			}
			continue
		}

		c, err := b.ensureClient(creds)
		if err != nil {
			b.setStatus(wire.ConnectorStatus{
				State: wire.ConnectorStateError,
				Error: t(b.lang(), "bot.bad_config", "error", err.Error()),
			})
			if !sleep(ctx, b.wake, 15*time.Second) {
				return
			}
			continue
		}

		b.setStatus(wire.ConnectorStatus{
			State:   wire.ConnectorStateOk,
			Account: accountLabel(b.lang(), creds),
			Via:     t(b.lang(), "bot.via"),
		})

		updates, err := c.GetUpdates(ctx, offset, 25)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var ae *APIError
			if errors.As(err, &ae) && ae.Unauthorized() {
				b.setStatus(wire.ConnectorStatus{
					State: wire.ConnectorStateNotSignedIn,
					Error: t(b.lang(), "bot.revoked"),
				})
				select {
				case <-ctx.Done():
					return
				case <-b.wake:
				}
				continue
			}
			b.setStatus(wire.ConnectorStatus{
				State:   wire.ConnectorStateError,
				Account: accountLabel(b.lang(), creds),
				Error:   t(b.lang(), "bot.retrying", "error", err.Error()),
			})
			if !sleep(ctx, b.wake, backoff) {
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}

		backoff = 2 * time.Second
		for _, upd := range updates {
			b.advanceOffset(ctx, upd.UpdateID+1)
			if upd.Message != nil {
				b.handleMessage(ctx, c, *creds, *upd.Message)
			}
		}
	}
}

func (b *Bot) ensureClient(creds *credentials) (*Client, error) {
	b.mu.Lock()
	c := b.client
	b.mu.Unlock()
	if c != nil {
		return c, nil
	}
	nc, err := b.opt.NewClient(creds.Token, creds.Proxy)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.client == nil {
		b.client = nc
	} else {
		nc = b.client
	}
	b.mu.Unlock()
	return nc, nil
}

func (b *Bot) advanceOffset(ctx context.Context, next int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if next > b.st.Offset {
		b.st.Offset = next
		b.saveLocked(ctx)
	}
}

// Logout clears all stored credentials and runtime state.
func (b *Bot) Logout(ctx context.Context) error {
	b.stopTyping()
	b.mu.Lock()
	b.st = state{}
	b.loaded = true
	b.client = nil
	b.pair = nil
	b.sendGen++
	b.mu.Unlock()
	err := b.secrets.Delete(ctx)
	b.setStatus(wire.ConnectorStatus{
		State: wire.ConnectorStateNotSignedIn,
		Error: t(b.lang(), "bot.not_bound"),
	})
	b.kick()
	return err
}

func accountLabel(l string, c *credentials) string {
	botName := "@" + c.BotUsername
	if c.BotUsername == "" {
		botName = "Bot"
	}
	if c.OwnerName != "" {
		return t(l, "bot.account", "bot", botName, "owner", c.OwnerName)
	}
	return botName
}

func sleep(ctx context.Context, wake <-chan struct{}, d time.Duration) bool {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-tm.C:
		return true
	}
}
