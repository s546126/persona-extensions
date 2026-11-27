package tgbot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	doneMost     = 256
	sendDeadline = 2 * time.Minute
)

// Program constructs the ext.Program for the Telegram Bot extension.
func Program(version string) ext.Program {
	p := &program{}
	return ext.Program{
		ID:      ExtID,
		Version: version,
		Init:    p.init,
		Namespaces: map[string]ext.Handler{
			ext.ConnectorNamespace: p.connector,
			ext.ChannelNamespace:   p.channel,
		},
	}
}

type program struct {
	options func(*Options)

	mu   sync.Mutex
	bot  *Bot
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
		gen := b.sendGeneration()
		res, err := p.sent.do(ctx, sp.Ref, sendKey(gen, sp), func() (wire.ChannelSendResult, error) {
			return b.Send(ctx, sp)
		})
		switch {
		case errors.Is(err, errCannotSend):
			return nil, ext.CannotSend(err.Error())
		case errors.Is(err, errStillSending):
			log.Printf("telegram-bot: send ref=%q timed out waiting for earlier send", sp.Ref)
			return nil, errors.New(t(b.lang(), "chat.send_failed"))
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

type channelHub struct{ hub *ext.Hub }

func (c channelHub) Receive(ctx context.Context, m wire.ChannelMessage) error {
	return c.hub.ChannelReceive(ctx, ID, m)
}

type hubSecrets struct{ hub *ext.Hub }

func (s hubSecrets) Load(ctx context.Context) ([]byte, error) { return s.hub.LoadSecret(ctx, ID) }
func (s hubSecrets) Save(ctx context.Context, data []byte) error {
	return s.hub.SaveSecret(ctx, ID, data)
}
func (s hubSecrets) Delete(ctx context.Context) error { return s.hub.DeleteSecret(ctx, ID) }

type sentRefs struct {
	mu    sync.Mutex
	sent  map[string]*sentRef
	order []string
}

type sentRef struct {
	key  string
	done chan struct{}
	res  wire.ChannelSendResult
	err  error
}

var errStillSending = errors.New("an earlier send of the ref is still running")

func (r *sentRefs) do(ctx context.Context, ref, key string, send func() (wire.ChannelSendResult, error)) (wire.ChannelSendResult, error) {
	r.mu.Lock()
	for {
		if r.sent == nil {
			r.sent = map[string]*sentRef{}
		}
		e := r.sent[ref]
		if e == nil {
			break
		}
		done := e.done
		same := e.key == key
		r.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return wire.ChannelSendResult{}, errStillSending
		}
		r.mu.Lock()
		if same && r.sent[ref] == e && e.err == nil {
			res := e.res
			r.mu.Unlock()
			return res, nil
		}
		if r.sent[ref] == e {
			delete(r.sent, ref)
			if i := indexOf(r.order, ref); i >= 0 {
				r.order = append(r.order[:i], r.order[i+1:]...)
			}
		}
	}
	e := &sentRef{key: key, done: make(chan struct{})}
	r.sent[ref] = e
	r.mu.Unlock()

	res, err := send()

	r.mu.Lock()
	e.res, e.err = res, err
	close(e.done)
	if err == nil {
		r.order = append(r.order, ref)
		for len(r.order) > doneMost {
			old := r.order[0]
			r.order = r.order[1:]
			if cur := r.sent[old]; cur != nil && cur.err == nil {
				select {
				case <-cur.done:
					delete(r.sent, old)
				default:
				}
			}
		}
	} else if r.sent[ref] == e {
		delete(r.sent, ref)
	}
	r.mu.Unlock()
	return res, err
}

func indexOf(s []string, target string) int {
	for i, v := range s {
		if v == target {
			return i
		}
	}
	return -1
}

func sendKey(gen uint64, p wire.ChannelSendParams) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatUint(gen, 10)))
	h.Write([]byte{0})
	h.Write([]byte(p.Kind))
	h.Write([]byte{0})
	h.Write([]byte(p.ReplyTo))
	h.Write([]byte{0})
	h.Write([]byte(p.Text))
	for _, img := range p.Images {
		h.Write([]byte{0})
		h.Write([]byte(img.Mime))
		h.Write([]byte{0})
		h.Write([]byte(img.Data))
	}
	return hex.EncodeToString(h.Sum(nil))
}
