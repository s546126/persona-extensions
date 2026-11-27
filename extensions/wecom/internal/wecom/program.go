package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// Capabilities are its connector's: a status (whether it answers someone, or what is missing) and a sign-in (the
// robot's credentials, then a pairing code). Nothing goes to the inbox or the knowledge base: a robot only sees what
// is sent to it.
var Capabilities = []string{wire.ConnectorCapabilityStatus, wire.ConnectorCapabilityLogin}

// Item is the connector it contributes, as initialize answers it.
func Item() wire.ConnectorItem {
	return wire.ConnectorItem{ID: ID, Name: Name, Capabilities: Capabilities}
}

// MaxText is how many characters one message to WeCom carries: the hub cuts longer answers into pieces this long
// (WeCom takes 20,480 bytes of Markdown; 4,000 characters stay under it even in CJK).
const MaxText = 4000

// Network is where it goes: the long connection (openws.work.weixin.qq.com) and where WeCom keeps message media.
var Network = []string{"*.qq.com", "*.myqcloud.com", "*.qpic.cn"}

// Channel is the channel it contributes (docs/extensions/channels.md in Persona): text, pictures, files and voice
// (WeCom's own transcript) come in, Markdown goes out, what agents ask is answered in text.
func Channel() wire.ChannelItem {
	return wire.ChannelItem{ID: ID, Name: Name, Kind: "im", Connector: ID, Bind: "fields",
		Description: "在企业微信里私聊智能机器人：填入机器人凭证并配对，消息进主对话，回答以 Markdown 发回", // i18n-ignore: as the hub registers it
		Capabilities: wire.ChannelCapabilities{ImagesIn: true, FilesIn: true, VoiceIn: true, Format: "markdown", MaxText: MaxText,
			Asks: "text", Push: true}}
}

// Program is the extension's program for the SDK (ext.Serve, ext.Main): the robot starts at initialize, with the
// user's language and the connector's secrets; channel.send, channel.typing and the connector's calls go to it.
func Program(version string) ext.Program {
	p := &program{}
	return ext.Program{ID: ID, Version: version, Init: p.init,
		Namespaces: map[string]ext.Handler{ext.ConnectorNamespace: p.connector, ext.ChannelNamespace: p.channel}}
}

type program struct {
	// options adjusts the robot's options before it starts (tests: a fake WeCom, a clock).
	options func(*Options)

	mu  sync.Mutex
	bot *Bot
	// sent are the refs of the latest channel.send done or still running: the hub sends one again when it did not
	// hear back, and WeCom keeps no idempotency key of its own.
	sent sentRefs
}

func (p *program) init(ctx context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bot == nil {
		opt := Options{Hub: channelHub{hub}, Lang: in.Hub.Locale}
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
// unknown_method.
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

// channel answers the channel namespace: channel.send (once per ref) and channel.typing (the robot shows no typing
// indicator: taken, nothing shown).
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
		res, err := p.sent.do(sp.Ref, func() (wire.ChannelSendResult, error) { return b.Send(ctx, sp) })
		if errors.Is(err, errCannotSend) {
			return nil, ext.CannotSend(err.Error())
		}
		return res, err
	case "typing":
		return ext.Empty{}, nil
	}
	return nil, ext.UnknownMethod("channel." + method)
}

// channelHub is the hub as the robot hands it the owner's messages.
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

// sentRefs runs each channel.send once by its ref (a failed one may go again). A send still running holds its ref,
// so the hub's retry of it (it timed out waiting) waits for that send instead of sending the message again.
type sentRefs struct {
	mu    sync.Mutex
	sent  map[string]*sentRef
	order []string // the refs sent successfully, oldest first
}

type sentRef struct {
	done chan struct{}
	res  wire.ChannelSendResult
	err  error
}

func (r *sentRefs) do(ref string, send func() (wire.ChannelSendResult, error)) (wire.ChannelSendResult, error) {
	r.mu.Lock()
	if prev, ok := r.sent[ref]; ok {
		r.mu.Unlock()
		<-prev.done
		return prev.res, prev.err
	}
	if r.sent == nil {
		r.sent = map[string]*sentRef{}
	}
	cur := &sentRef{done: make(chan struct{})}
	r.sent[ref] = cur
	r.mu.Unlock()
	cur.res, cur.err = send()
	r.mu.Lock()
	var k kept
	if cur.err != nil && !errors.As(cur.err, &k) {
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
