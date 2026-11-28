// Package api is a small client of WeCom's smart robot (智能机器人) long connection: a WebSocket the app opens to
// WeCom (so the hub needs no public address), subscribed with the robot's BotID and long-connection Secret. Over it
// come the messages people send the robot (aibot_msg_callback) and events (aibot_event_callback); over it go
// heartbeats, replies and messages the robot sends on its own (aibot_send_msg). Every request carries a req_id that
// its answer ({headers: {req_id}, errcode, errmsg}) repeats. Media come as a URL and a per-message AES key
// (media.go). Secrets and message contents are never logged.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Origin is WeCom's long-connection endpoint.
const Origin = "wss://openws.work.weixin.qq.com"

// Commands.
const (
	cmdSubscribe = "aibot_subscribe"
	cmdPing      = "ping"
	cmdSend      = "aibot_send_msg"
	// CmdMessage and CmdEvent are what WeCom sends: a message to the robot, an event.
	CmdMessage = "aibot_msg_callback"
	CmdEvent   = "aibot_event_callback"
	// EventKicked: another connection subscribed the same robot, and this one is closed.
	EventKicked = "disconnected_event"
)

const (
	readLimit   = 4 << 20
	dialTimeout = 15 * time.Second
)

// answerWait is how long a request waits for WeCom's answer unless Options say otherwise.
const answerWait = 10 * time.Second

// PingEvery is how often the connection sends a heartbeat (WeCom asks for every 30 seconds); tests shorten it.
var PingEvery = 30 * time.Second

// Options configure a Client. URL and HTTP are what tests replace.
type Options struct {
	BotID  string
	Secret string // the long-connection Secret (not the URL-callback Token / EncodingAESKey)
	URL    string // Origin when empty; must be wss
	// HTTP makes the WebSocket dial and media downloads (its transport, so HTTPS_PROXY is used, when nil). It must
	// have no Timeout: the WebSocket dial refuses one.
	HTTP *http.Client
	// DialTimeout bounds connecting and subscribing (dialTimeout when 0); tests shorten it.
	DialTimeout time.Duration
	// AnswerWait is how long a request waits for WeCom's answer (answerWait when 0); tests shorten it.
	AnswerWait time.Duration
	// MediaHosts are the hosts media may be downloaded from (a host or "*.suffix"); the defaults when empty.
	MediaHosts []string
}

// Client talks to WeCom for one robot.
type Client struct{ opt Options }

func New(opt Options) (*Client, error) {
	if strings.TrimSpace(opt.BotID) == "" || strings.TrimSpace(opt.Secret) == "" {
		return nil, errors.New("wecom: a BotID and a Secret are needed")
	}
	if opt.URL == "" {
		opt.URL = Origin
	}
	u, err := url.Parse(opt.URL)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("wecom: the long-connection address must be wss: %q", opt.URL)
	}
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{}
	}
	if opt.DialTimeout <= 0 {
		opt.DialTimeout = dialTimeout
	}
	if opt.AnswerWait <= 0 {
		opt.AnswerWait = answerWait
	}
	if len(opt.MediaHosts) == 0 {
		opt.MediaHosts = DefaultMediaHosts
	}
	return &Client{opt: opt}, nil
}

// Error is WeCom's refusal of a request: its errcode and errmsg (never a secret).
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("WeCom answered errcode %d: %s", e.Code, e.Message)
}

// ErrNotSent: the request never left (the connection would not take it); sending it again cannot duplicate it.
var ErrNotSent = errors.New("not sent")

// ErrNoAnswer: the request went out but its answer did not come back (a timeout, the connection closing). WeCom may
// have taken it: an unknown outcome, never a reason to send it again.
var ErrNoAnswer = errors.New("sent, but WeCom's answer did not come back")

// ErrKicked: another connection subscribed the same robot (another program, or another hub), and WeCom closed this
// one. Connecting again would take the robot back, and the two would keep closing each other.
var ErrKicked = errors.New("another connection took over this WeCom robot")

// Unauthorized says WeCom refused the robot's BotID or Secret when subscribing.
func Unauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && errors.Is(err, errSubscribe)
}

var errSubscribe = errors.New("subscribing")

// Frame is one frame of the long connection, either way.
type Frame struct {
	Cmd     string            `json:"cmd,omitempty"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body,omitempty"`
	ErrCode *int              `json:"errcode,omitempty"`
	ErrMsg  string            `json:"errmsg,omitempty"`
}

// ReqID is the frame's req_id.
func (f Frame) ReqID() string { return f.Headers["req_id"] }

func newReqID(cmd string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return cmd + "_" + hex.EncodeToString(b[:])
}

// Session is one subscribed connection: what the robot sends goes through it.
type Session struct {
	conn *websocket.Conn
	wait time.Duration // how long a request waits for its answer

	mu      sync.Mutex
	waiting map[string]chan Frame
}

func (s *Session) write(ctx context.Context, f Frame) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.wait)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, raw)
}

// request sends cmd with body and waits for its answer: nil when errcode is 0, *Error otherwise.
func (s *Session) request(ctx context.Context, cmd string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	id := newReqID(cmd)
	ch := make(chan Frame, 1)
	s.mu.Lock()
	if s.waiting == nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: the WeCom connection is closed", ErrNotSent)
	}
	s.waiting[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.waiting != nil {
			delete(s.waiting, id)
		}
		s.mu.Unlock()
	}()
	if body == nil {
		raw = nil
	}
	if err := s.write(ctx, Frame{Cmd: cmd, Headers: map[string]string{"req_id": id}, Body: raw}); err != nil {
		// a failed write may still have put the frame on the wire: unknown, never sent again
		return fmt.Errorf("%w: writing to WeCom: %w", ErrNoAnswer, err)
	}
	t := time.NewTimer(s.wait)
	defer t.Stop()
	select {
	case f, ok := <-ch:
		if !ok {
			return fmt.Errorf("%w: the WeCom connection closed before it answered", ErrNoAnswer)
		}
		if f.ErrCode != nil && *f.ErrCode != 0 {
			return &Error{Code: *f.ErrCode, Message: errText(f.ErrMsg)}
		}
		return nil
	case <-t.C:
		return fmt.Errorf("%w: WeCom did not answer %s in %s", ErrNoAnswer, cmd, s.wait)
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrNoAnswer, ctx.Err())
	}
}

// answer hands an answer frame to its waiting request; false when nobody waits for it.
func (s *Session) answer(f Frame) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.waiting[f.ReqID()]
	if ok {
		ch <- f
		delete(s.waiting, f.ReqID())
	}
	return ok
}

func (s *Session) close() {
	s.mu.Lock()
	for _, ch := range s.waiting {
		close(ch)
	}
	s.waiting = nil
	s.mu.Unlock()
}

// SendMarkdown sends Markdown to one person in their one-to-one chat with the robot (aibot_send_msg). WeCom allows
// it only after the person wrote to the robot, and keeps no idempotency key: the caller sends a piece once.
func (s *Session) SendMarkdown(ctx context.Context, userID, content string) error {
	return s.request(ctx, cmdSend, map[string]any{
		"chatid": userID, "chat_type": 1, "msgtype": "markdown", "markdown": map[string]string{"content": content},
	})
}

// Subscribe opens a connection and subscribes the robot, then closes it: a check of the BotID and Secret.
func (c *Client) Subscribe(ctx context.Context) error {
	s, err := c.open(ctx)
	if err != nil {
		return err
	}
	s.close()
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
	return nil
}

// open dials and subscribes, reading only until the subscription is answered.
func (c *Client) open(ctx context.Context) (*Session, error) {
	// the dial's context bounds the handshake and the subscription only: the established connection outlives it
	dctx, cancel := context.WithTimeout(ctx, c.opt.DialTimeout)
	defer cancel()
	base := c.opt.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc := *c.opt.HTTP
	hc.Transport = wsCasing{base}
	conn, resp, err := websocket.Dial(dctx, c.opt.URL, &websocket.DialOptions{HTTPClient: &hc})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to WeCom: %w", err)
	}
	conn.SetReadLimit(readLimit)
	s := &Session{conn: conn, wait: c.opt.AnswerWait, waiting: map[string]chan Frame{}}
	id := newReqID(cmdSubscribe)
	body, _ := json.Marshal(map[string]string{"bot_id": c.opt.BotID, "secret": c.opt.Secret})
	if err := s.write(dctx, Frame{Cmd: cmdSubscribe, Headers: map[string]string{"req_id": id}, Body: body}); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("subscribing to WeCom: %w", err)
	}
	for {
		_, data, err := conn.Read(dctx)
		if err != nil {
			conn.CloseNow()
			return nil, fmt.Errorf("subscribing to WeCom: %w", err)
		}
		var f Frame
		if json.Unmarshal(data, &f) != nil || f.ReqID() != id {
			continue
		}
		if f.ErrCode != nil && *f.ErrCode != 0 {
			conn.CloseNow()
			return nil, fmt.Errorf("%w: %w", errSubscribe, &Error{Code: *f.ErrCode, Message: errText(f.ErrMsg)})
		}
		return s, nil
	}
}

// Run holds one subscribed connection until it fails, another connection takes the robot over (ErrKicked) or ctx
// ends. connected is called with the session once subscribed; handle is called for each message and event, one at
// a time and in the order they came, and must not block for long (it holds up the reading).
func (c *Client) Run(ctx context.Context, connected func(*Session), handle func(Frame)) error {
	s, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer s.conn.CloseNow()
	defer s.close()
	if connected != nil {
		connected(s)
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	go func() {
		t := time.NewTicker(PingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := s.request(ctx, cmdPing, nil); err != nil && ctx.Err() == nil {
				stop(fmt.Errorf("WeCom stopped answering heartbeats: %w", err))
				return
			}
		}
	}()
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return fmt.Errorf("reading from WeCom: %w", err)
		}
		var f Frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.Cmd {
		case "":
			s.answer(f)
		case CmdEvent:
			var e struct {
				Event struct {
					Type string `json:"eventtype"`
				} `json:"event"`
			}
			_ = json.Unmarshal(f.Body, &e)
			if e.Event.Type == EventKicked {
				return ErrKicked
			}
			if handle != nil {
				handle(f)
			}
		default:
			if handle != nil {
				handle(f)
			}
		}
	}
}

// wsCasing writes the handshake's Sec-WebSocket-* header names as RFC 6455 spells them. WeCom's endpoint matches
// them case-sensitively and answers 404 to Go's canonical "Sec-Websocket-Key" (seen 2026-11); header names are
// case-insensitive in HTTP, so the spelling changes nothing for any other server.
type wsCasing struct{ rt http.RoundTripper }

func (w wsCasing) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range r.Header {
		if rest, ok := strings.CutPrefix(k, "Sec-Websocket-"); ok {
			delete(r.Header, k)
			r.Header["Sec-WebSocket-"+rest] = v
		}
	}
	return w.rt.RoundTrip(r)
}

// ErrText is err for people: WeCom's own refusal as its trimmed errmsg, anything else as its text.
func ErrText(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return fmt.Sprintf("%d %s", e.Code, e.Message)
	}
	return Redact(err)
}

// errText is WeCom's errmsg for people: what comes before its ", hint: [...] from ip: ..." tail (the hint is a
// request id, the IP the hub's public address).
func errText(msg string) string {
	if i := strings.Index(msg, ", hint:"); i >= 0 {
		msg = msg[:i]
	}
	if i := strings.Index(msg, "from ip"); i >= 0 {
		msg = strings.TrimRight(msg[:i], ", ")
	}
	return clip(strings.TrimSpace(msg), 200)
}

func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > n {
		if n == 1 {
			return "…"
		}
		return string(r[:n-1]) + "…"
	}
	return s
}
