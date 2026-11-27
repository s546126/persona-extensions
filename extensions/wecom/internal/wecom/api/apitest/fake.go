// Package apitest is a fake WeCom smart robot long connection for tests: subscribing, heartbeats, aibot_send_msg,
// messages and events the test pushes, and encrypted media. Everything in it is synthetic.
package apitest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nwverse/persona-extensions/extensions/wecom/internal/wecom/api"
)

// The fake robot's credentials.
const (
	BotID  = "synthetic-bot-id"
	Secret = "synthetic-secret"
)

// Sent is a Markdown message the robot sent to someone.
type Sent struct {
	UserID string
	Text   string
}

// Fake is the fake long connection.
type Fake struct {
	Server *httptest.Server

	mu    sync.Mutex
	media map[string][]byte // encrypted, by name
	names map[string]string // the file name its download gives, by name
	// LoseAnswers is how many more aibot_send_msg are taken (recorded as delivered) but never answered.
	LoseAnswers int
	// LoseFrom loses the answers of every send once this many were delivered, the current one included (0: never).
	LoseFrom int
	// Refuse answers every aibot_send_msg with this errcode (0: none).
	Refuse     int
	subscribes int
	pings      int
	sent       []Sent
	conn       *websocket.Conn
	connCtx    context.Context
	up         chan struct{}
}

func New(t testing.TB) *Fake {
	f := &Fake{media: map[string][]byte{}, names: map[string]string{}, up: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", f.connect)
	mux.HandleFunc("GET /media/{name}", f.file)
	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
	})
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(func() {
		f.mu.Lock()
		if f.conn != nil {
			_ = f.conn.CloseNow()
		}
		f.mu.Unlock()
		f.Server.Close()
	})
	return f
}

// Set changes the fake's behavior (Refuse, LoseAnswers, LoseFrom) under its lock: its connection reads them.
func (f *Fake) Set(change func(f *Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// Options are a client's options for the fake (credentials apart).
func (f *Fake) Options(botID, secret string) api.Options {
	host := strings.TrimPrefix(f.Server.URL, "https://")
	h, _, _ := strings.Cut(host, ":")
	return api.Options{BotID: botID, Secret: secret, URL: "wss://" + host + "/", HTTP: f.Server.Client(), MediaHosts: []string{h},
		AnswerWait: 300 * time.Millisecond} // the fake answers at once, or never
}

// Key is the synthetic AES key for name, as WeCom gives it (base64, no padding), and its bytes.
func Key(name string) (string, []byte) {
	k := sha256.Sum256([]byte("synthetic-key-" + name))
	return base64.RawStdEncoding.EncodeToString(k[:]), k[:]
}

// Media keeps plain as encrypted media under name, and answers its URL (http, as WeCom may give it) and aeskey.
func (f *Fake) Media(name string, plain []byte) (url, aesKey string) {
	s, k := Key(name)
	f.mu.Lock()
	f.media[name] = api.Encrypt(plain, k)
	f.mu.Unlock()
	return "http://" + strings.TrimPrefix(f.Server.URL, "https://") + "/media/" + name, s
}

// MediaNamed is Media whose download gives fileName in its Content-Disposition, as WeCom does for files.
func (f *Fake) MediaNamed(name, fileName string, plain []byte) (url, aesKey string) {
	f.mu.Lock()
	f.names[name] = fileName
	f.mu.Unlock()
	return f.Media(name, plain)
}

func (f *Fake) file(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	data, ok := f.media[r.PathValue("name")]
	fileName := f.names[r.PathValue("name")]
	f.mu.Unlock()
	if fileName != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fileName}))
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(data)
}

func (f *Fake) connect(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx := r.Context()
	answer := func(reqID string, code int, msg string) {
		raw, _ := json.Marshal(map[string]any{"headers": map[string]string{"req_id": reqID}, "errcode": code, "errmsg": msg})
		_ = c.Write(ctx, websocket.MessageText, raw)
	}
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var fr struct {
			Cmd     string            `json:"cmd"`
			Headers map[string]string `json:"headers"`
			Body    json.RawMessage   `json:"body"`
		}
		if json.Unmarshal(data, &fr) != nil {
			continue
		}
		id := fr.Headers["req_id"]
		switch fr.Cmd {
		case "aibot_subscribe":
			var b struct{ BotID, Secret string }
			var raw map[string]string
			_ = json.Unmarshal(fr.Body, &raw)
			b.BotID, b.Secret = raw["bot_id"], raw["secret"]
			if b.BotID != BotID || b.Secret != Secret {
				answer(id, 853000, "invalid bot_id or secret, hint: [0000000000000000000000000], from ip: 192.0.2.1, more info at https://open.work.weixin.qq.com/devtool/query?e=853000")
				continue
			}
			f.mu.Lock()
			old := f.conn
			f.subscribes++
			f.conn, f.connCtx = c, ctx
			f.mu.Unlock()
			if old != nil && old != c {
				_ = old.CloseNow()
			}
			answer(id, 0, "ok")
			f.up <- struct{}{}
		case "ping":
			f.mu.Lock()
			f.pings++
			f.mu.Unlock()
			answer(id, 0, "ok")
		case "aibot_send_msg":
			var b struct {
				ChatID   string `json:"chatid"`
				ChatType int    `json:"chat_type"`
				MsgType  string `json:"msgtype"`
				Markdown struct {
					Content string `json:"content"`
				} `json:"markdown"`
			}
			_ = json.Unmarshal(fr.Body, &b)
			f.mu.Lock()
			refuse := f.Refuse
			if refuse == 0 && b.ChatType == 1 && b.MsgType == "markdown" {
				f.sent = append(f.sent, Sent{UserID: b.ChatID, Text: b.Markdown.Content})
			}
			lose := refuse == 0 && (f.LoseAnswers > 0 || f.LoseFrom > 0 && len(f.sent) >= f.LoseFrom)
			if lose && f.LoseAnswers > 0 {
				f.LoseAnswers--
			}
			f.mu.Unlock()
			if lose {
				continue // delivered; the acknowledgement is lost
			}
			if refuse != 0 {
				answer(id, refuse, "refused")
			} else {
				answer(id, 0, "ok")
			}
		default:
			answer(id, 40008, "unknown cmd")
		}
	}
}

// WaitConnected waits for the next subscription.
func (f *Fake) WaitConnected(t testing.TB) {
	t.Helper()
	select {
	case <-f.up:
	case <-time.After(5 * time.Second):
		t.Fatal("no subscription")
	}
}

// Push sends a frame on the subscribed connection.
func (f *Fake) Push(t testing.TB, cmd string, body any) {
	t.Helper()
	f.mu.Lock()
	c, ctx := f.conn, f.connCtx
	f.mu.Unlock()
	if c == nil {
		t.Fatal("no connection to push on")
	}
	raw, _ := json.Marshal(map[string]any{"cmd": cmd, "headers": map[string]string{"req_id": "push-" + time.Now().Format("150405.000000")}, "body": body})
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("pushing: %v", err)
	}
}

// Message pushes a one-to-one message from userID; extra fields (msgtype and its object) are merged in.
func (f *Fake) Message(t testing.TB, msgID, userID string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"msgid": msgID, "aibotid": BotID, "chattype": "single", "from": map[string]string{"userid": userID}}
	for k, v := range extra {
		body[k] = v
	}
	f.Push(t, api.CmdMessage, body)
}

// Text pushes a one-to-one text message.
func (f *Fake) Text(t testing.TB, msgID, userID, text string) {
	t.Helper()
	f.Message(t, msgID, userID, map[string]any{"msgtype": "text", "text": map[string]string{"content": text}})
}

// Kick tells the connection another one took the robot over.
func (f *Fake) Kick(t testing.TB) {
	t.Helper()
	f.Push(t, api.CmdEvent, map[string]any{"msgtype": "event", "event": map[string]string{"eventtype": api.EventKicked}})
}

func (f *Fake) Sent() []Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Sent(nil), f.sent...)
}

func (f *Fake) Subscribes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscribes
}

// Eventually waits until ok holds.
func Eventually(t testing.TB, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
