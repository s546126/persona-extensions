package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// Capabilities are its connector's: a status (bound to whose WeChat, or not) and a sign-in (a QR code to scan in
// WeChat). Nothing goes to the inbox or the knowledge base: iLink only carries the chat with the bot.
var Capabilities = []string{wire.ConnectorCapabilityStatus, wire.ConnectorCapabilityLogin}

// Item is the connector it contributes, as initialize answers it.
func Item() wire.ConnectorItem {
	return wire.ConnectorItem{ID: ID, Name: Name, Capabilities: Capabilities}
}

// MaxText is how many characters one WeChat message takes (ilink.MaxText).
const MaxText = 3500

// Network is where it goes: iLink and WeChat's media CDN.
var Network = []string{"*.weixin.qq.com", "*.wechat.com"}

// Channel is the channel it contributes (docs/extensions/channels.md): pictures, documents, videos and voice (WeChat's
// own transcript, or decrypted audio delivered as a file when WeChat sends no transcript) come in, plain text goes out
// in pieces WeChat takes and an answer's pictures as WeChat pictures, what agents ask is answered in text, "typing…"
// while a turn goes on.
func Channel() wire.ChannelItem {
	return wire.ChannelItem{ID: ID, Name: Name, Kind: "im", Connector: ID, Bind: "qr",
		Description: "在个人微信里私聊机器人：扫码绑定，消息进主对话，回答以文字和图片发回", // i18n-ignore: as the hub registers it
		Capabilities: wire.ChannelCapabilities{ImagesIn: true, ImagesOut: true, FilesIn: true, VoiceIn: true, Format: "plain",
			MaxText: MaxText, Asks: "text", Push: true, Typing: true}}
}

// Manifest describes a synthetic single-platform package for protocol tests.
func Manifest(version, command string) wire.Manifest {
	return wire.Manifest{ManifestVersion: 1, ID: ID, Name: Name, Version: version,
		Description: "微信机器人：在个人微信里和 Persona 对话", // i18n-ignore: shipped extension metadata
		Engines:     &wire.Engines{ExtensionProtocol: ext.Protocol},
		Runtime:     &wire.Runtime{Command: mustJSON(command)},
		Permissions: &wire.Permissions{Network: Network},
		Contributes: &wire.Contributes{Connectors: []wire.ConnectorItem{Item()}, Channel: []wire.ChannelItem{Channel()}}}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Program is the plugin's program for the SDK (ext.Serve, ext.Main): the bot starts at initialize, with the user's
// language and the connector's secrets; channel.send, channel.typing and the connector's calls go to it.
func Program(version string) ext.Program {
	p := &program{agent: "Persona/" + version}
	return ext.Program{ID: ID, Version: version, Init: p.init,
		Namespaces: map[string]ext.Handler{ext.ConnectorNamespace: p.connector, ext.ChannelNamespace: p.channel}}
}

type program struct {
	agent string
	// options adjusts the bot's options before it starts (tests: a fake iLink, a clock).
	options func(*Options)

	mu  sync.Mutex
	bot *Bot
	// sent are the refs of the latest channel.send done or still running, and what each answered: the hub sends one
	// again when it did not hear back.
	sent sentRefs
}

func (p *program) init(ctx context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bot == nil {
		opt := Options{Hub: channelHub{hub}, Agent: p.agent, Lang: in.Hub.Locale, DataDir: in.Info().DataDir}
		if p.options != nil {
			p.options(&opt)
		}
		p.bot = New(opt, hubSecrets{hub})
		go p.bot.Run(ctx)
	}
	return map[string]any{"connectors": []wire.ConnectorItem{Item()}}, nil
}

func (p *program) theBot() (*Bot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bot == nil {
		return nil, &ext.Error{Code: wire.ErrorCodeNotInitialized, Message: "not initialized"}
	}
	return p.bot, nil
}

// connector answers the connector namespace: status, login.step, logout; tools.list and actions.list are empty (the
// hub asks every connector for them); another of the protocol's methods is no_capability and one it does not have
// unknown_method, as connector/serve answers them.
func (p *program) connector(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if c := ext.Contribution(params); c != "" && c != ID {
		return nil, ext.NoContribution(c)
	}
	b, err := p.theBot()
	if err != nil {
		return nil, err
	}
	switch method {
	case "manifest":
		return wire.ConnectorManifest{Protocol: 2, ID: ID, Name: Name, Capabilities: Capabilities}, nil
	case "status":
		return b.Status(ctx), nil
	case "login.step":
		var lp wire.ConnectorLoginParams
		if err := json.Unmarshal(params, &lp); err != nil {
			return nil, err
		}
		return b.LoginStep(ctx, lp.Step, lp.Values)
	case "logout":
		return ext.Empty{}, b.Logout(ctx)
	case "tools.list", "actions.list":
		return wire.ConnectorTools{}, nil
	}
	if c, ok := wire.Calls[ext.ConnectorNamespace+"."+method]; ok && c.From == "hub" {
		return nil, &ext.Error{Code: wire.ErrorCodeNoCapability, Message: fmt.Sprintf("%s has no %s", Name, method)}
	}
	return nil, ext.UnknownMethod(ext.ConnectorNamespace + "." + method)
}

// channel answers the channel namespace: channel.send (once per ref and content) and channel.typing.
func (p *program) channel(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if c := ext.Contribution(params); c != "" && c != ID {
		return nil, &ext.Error{Code: wire.ErrorCodeNoContribution, Message: "no channel " + c}
	}
	b, err := p.theBot()
	if err != nil {
		return nil, err
	}
	switch method {
	case "send":
		var sp wire.ChannelSendParams
		if err := json.Unmarshal(params, &sp); err != nil {
			return nil, err
		}
		if sp.Ref == "" {
			return nil, errors.New("channel.send without a ref")
		}
		ctx, cancel := context.WithTimeout(ctx, sendDeadline) // waiting for an earlier send of the ref counts too
		defer cancel()
		gen := b.sendGeneration() // the binding the call is for: after waiting for the ref, it goes there or nowhere
		res, err := p.sent.do(ctx, sp.Ref, sendKey(gen, sp), func() (wire.ChannelSendResult, error) { return b.sendIn(ctx, gen, sp) })
		switch {
		case errors.Is(err, errCannotSend), errors.Is(err, errRebound):
			return nil, ext.CannotSend(err.Error())
		case err != nil && b.sendGeneration() != gen:
			// Failed (out of time waiting for the ref, or a send that ran across the change) after the WeChat the call
			// came in for was unbound: the hub's retry would come in for the WeChat bound now, so it hears cannot_send.
			log.Printf("wechat: send reply_to=%q ref=%q failed after the WeChat it came in for was unbound: %v", sp.ReplyTo, sp.Ref, err)
			return nil, ext.CannotSend(errRebound.Error())
		case errors.Is(err, errStillSending):
			log.Printf("wechat: send reply_to=%q ref=%q stopped: out of time (%s at most) waiting for the ref's earlier send",
				sp.ReplyTo, sp.Ref, sendDeadline)
			return nil, sendFailed{b.lang()}
		}
		return res, err
	case "typing":
		var tp wire.ChannelTypingParams
		if err := json.Unmarshal(params, &tp); err != nil {
			return nil, err
		}
		return ext.Empty{}, b.Typing(ctx, tp.On)
	}
	return nil, ext.UnknownMethod("channel." + method)
}

// channelHub is the hub as the bot hands it the owner's messages.
type channelHub struct{ hub *ext.Hub }

func (c channelHub) Receive(ctx context.Context, m wire.ChannelMessage) error {
	return c.hub.ChannelReceive(ctx, ID, m)
}

// hubSecrets is the connector's sealed blob in the hub.
type hubSecrets struct{ hub *ext.Hub }

func (s hubSecrets) Load(ctx context.Context) ([]byte, error) { return s.hub.LoadSecret(ctx, ID) }
func (s hubSecrets) Save(ctx context.Context, data []byte) error {
	return s.hub.SaveSecret(ctx, ID, data)
}
func (s hubSecrets) Delete(ctx context.Context) error { return s.hub.DeleteSecret(ctx, ID) }

// doneMost is how many sent refs the program remembers.
const doneMost = 256

// sentRefs runs each channel.send once by its ref and key (sendKey: the binding and the content; a failed one may
// go again). A send still running holds its ref, so the hub's retry of it (it timed out waiting) waits for that send
// instead of sending the message again. The same ref with another key is another message: it waits for a send of the
// ref still running, then goes, and replaces what was remembered for the ref. A wait ends with ctx: errStillSending.
type sentRefs struct {
	mu    sync.Mutex
	sent  map[string]*sentRef
	order []string // the refs sent successfully, oldest first
	// waiting, when set (tests), is told the ref each time a send starts waiting for an earlier send of it.
	waiting func(ref string)
}

type sentRef struct {
	key  string // sendKey
	done chan struct{}
	res  wire.ChannelSendResult
	err  error
}

// errStillSending: the call's time ran out waiting for an earlier send of its ref.
var errStillSending = errors.New("an earlier send of the ref is still running")

func (r *sentRefs) do(ctx context.Context, ref, key string, send func() (wire.ChannelSendResult, error)) (wire.ChannelSendResult, error) {
	r.mu.Lock()
	for {
		prev, ok := r.sent[ref]
		if !ok {
			break
		}
		select {
		case <-prev.done: // sent: the same content was sent; other content is another message
			if prev.key == key {
				r.mu.Unlock()
				return prev.res, prev.err
			}
			delete(r.sent, ref)
			r.order = slices.DeleteFunc(r.order, func(o string) bool { return o == ref })
			continue
		default:
		}
		r.mu.Unlock()
		if r.waiting != nil {
			r.waiting(ref)
		}
		select {
		case <-prev.done:
		case <-ctx.Done():
			return wire.ChannelSendResult{}, errStillSending
		}
		if prev.key == key {
			return prev.res, prev.err
		}
		r.mu.Lock() // other content: look again, it goes after what holds the ref now
	}
	if r.sent == nil {
		r.sent = map[string]*sentRef{}
	}
	cur := &sentRef{key: key, done: make(chan struct{})}
	r.sent[ref] = cur
	r.mu.Unlock()
	cur.res, cur.err = send()
	r.mu.Lock()
	if cur.err != nil {
		delete(r.sent, ref) // forget a failed send before waking its waiters, so the ref may be sent again
	} else {
		r.order = append(r.order, ref)
		if len(r.order) > doneMost {
			delete(r.sent, r.order[0])
			r.order = r.order[1:]
		}
	}
	r.mu.Unlock()
	close(cur.done)
	return cur.res, cur.err
}
