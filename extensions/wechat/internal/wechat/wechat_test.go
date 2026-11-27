package wechat

import (
	"bytes"
	"cmp"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wechat/internal/wechat/ilink"
	"github.com/nwverse/persona/sdk/go/wire"
)

// memSecrets keeps the sealed blob in memory.
type memSecrets struct {
	mu   sync.Mutex
	data []byte
}

func (s *memSecrets) Load(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data, nil
}
func (s *memSecrets) Save(_ context.Context, d []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = append([]byte(nil), d...)
	return nil
}
func (s *memSecrets) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = nil
	return nil
}

func (s *memSecrets) state(t *testing.T) state {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st state
	if len(s.data) > 0 {
		if err := json.Unmarshal(s.data, &st); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// fakeILink is iLink: QR statuses as scripted, messages as queued, what the bot sends recorded.
type fakeILink struct {
	t         *testing.T
	srv       *httptest.Server
	mu        sync.Mutex
	statuses  []map[string]any
	queue     []map[string]any
	seq       int
	replaced  bool
	sent      []sentText
	typing    []bool
	downloads map[string][]byte
	// failSends: how many sendmessage calls to keep and then answer HTTP 500 (the answer lost on the way back).
	failSends int
	// sendID is the server message_id answered for a successful send.
	sendID string
	// failedSend signals that a scripted HTTP 500 response has been written, before the retry wait.
	failedSend chan struct{}
	clientIDs  []string          // every sendmessage's client_id, repeated ones too
	kept       map[string]bool   // client_ids iLink has: a repeat is not posted again
	hosts      []string          // hosts requested through RoundTrip
	uploadReq  []map[string]any  // getuploadurl's requests
	uploads    map[string][]byte // what the CDN took, by the download reference it answered
	pictures   []sentPicture
	// failUploads: how many CDN uploads to answer HTTP 500, once okUploads uploads were kept.
	failUploads int
	okUploads   int
	// answers are what the next calls of a path answer, one each, before the rest of serve; the zero fakeAnswer lets
	// a call be answered as usual. A sendmessage answered so is not kept.
	answers map[string][]fakeAnswer
	// requests are every request's host (as the bot named it), path and Authorization.
	requests []fakeRequest
}

// fakeAnswer is an HTTP status (200 when 0) and a JSON body (none when nil); hang answers nothing until the caller
// gives up.
type fakeAnswer struct {
	status int
	body   map[string]any
	hang   bool
}

type fakeRequest struct{ Host, Path, Auth string }

type sentText struct{ To, Context, Text string }

// sentPicture is an image_item the bot sent: where its picture is and its key as sent (base64 of the hex key).
type sentPicture struct {
	To, Context, Query, AESKey string
	ItemKey                    string // the image_item's own aeskey: the hex key
	EncryptType, MidSize       float64
}

func newFakeILink(t *testing.T) *fakeILink {
	f := &fakeILink{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeILink) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.hosts = append(f.hosts, r.URL.Host)
	f.requests = append(f.requests, fakeRequest{Host: r.URL.Host, Path: r.URL.Path, Auth: r.Header.Get("Authorization")})
	f.mu.Unlock()
	u, _ := url.Parse(f.srv.URL)
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host, r2.Host = u.Scheme, u.Host, u.Host
	return http.DefaultTransport.RoundTrip(r2)
}

func (f *fakeILink) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	out := map[string]any{"ret": 0}
	f.mu.Lock()
	if as := f.answers[r.URL.Path]; len(as) > 0 {
		a := as[0]
		f.answers[r.URL.Path] = as[1:]
		if a.hang {
			f.mu.Unlock()
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		if a.status != 0 || a.body != nil {
			switch r.URL.Path {
			case "/ilink/bot/sendmessage":
				f.clientIDs = append(f.clientIDs, body["msg"].(map[string]any)["client_id"].(string))
			case "/ilink/bot/getuploadurl":
				f.uploadReq = append(f.uploadReq, body)
			}
			f.mu.Unlock()
			w.WriteHeader(cmp.Or(a.status, http.StatusOK))
			if a.body != nil {
				_ = json.NewEncoder(w).Encode(a.body)
			}
			return
		}
	}
	switch r.URL.Path {
	case "/ilink/bot/get_bot_qrcode":
		out = map[string]any{"qrcode": "QC", "qrcode_img_content": "https://liteapp.weixin.qq.com/q/example"}
	case "/ilink/bot/get_qrcode_status":
		out = map[string]any{"status": "wait"}
		if len(f.statuses) > 0 {
			out, f.statuses = f.statuses[0], f.statuses[1:]
		}
	case "/ilink/bot/getupdates":
		if f.replaced {
			out = map[string]any{"errcode": -14}
			break
		}
		msgs := f.queue
		f.queue = nil
		if len(msgs) == 0 {
			f.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			f.mu.Lock()
		}
		f.seq++
		out = map[string]any{"ret": 0, "msgs": msgs, "get_updates_buf": "cursor-" + strconv.Itoa(f.seq)}
	case "/ilink/bot/sendmessage":
		msg := body["msg"].(map[string]any)
		id := msg["client_id"].(string)
		f.clientIDs = append(f.clientIDs, id)
		if !f.kept[id] {
			if f.kept == nil {
				f.kept = map[string]bool{}
			}
			f.kept[id] = true
			item := msg["item_list"].([]any)[0].(map[string]any)
			if pic, ok := item["image_item"].(map[string]any); ok {
				media := pic["media"].(map[string]any)
				f.pictures = append(f.pictures, sentPicture{To: msg["to_user_id"].(string), Context: msg["context_token"].(string),
					Query: media["encrypt_query_param"].(string), AESKey: media["aes_key"].(string), ItemKey: fmt.Sprint(pic["aeskey"]),
					EncryptType: media["encrypt_type"].(float64), MidSize: pic["mid_size"].(float64)})
			} else {
				f.sent = append(f.sent, sentText{To: msg["to_user_id"].(string), Context: msg["context_token"].(string),
					Text: item["text_item"].(map[string]any)["text"].(string)})
			}
		}
		if f.failSends > 0 {
			f.failSends--
			failed := f.failedSend
			f.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			if failed != nil {
				failed <- struct{}{}
			}
			return
		}
		if f.sendID != "" {
			out["message_id"] = f.sendID
		}
	case "/ilink/bot/getconfig":
		out["typing_ticket"] = "TT"
	case "/ilink/bot/sendtyping":
		f.typing = append(f.typing, body["status"] == float64(1))
	case "/c2c/download":
		data := f.downloads[r.URL.Query().Get("encrypted_query_param")]
		f.mu.Unlock()
		if data == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
		return
	case "/ilink/bot/getuploadurl":
		f.uploadReq = append(f.uploadReq, body)
		out["upload_param"] = "UP-" + strconv.Itoa(len(f.uploadReq))
	case "/c2c/upload":
		if f.okUploads > 0 {
			f.okUploads--
		} else if f.failUploads > 0 {
			f.failUploads--
			f.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		ref := "DL-" + r.URL.Query().Get("encrypted_query_param") + "-" + r.URL.Query().Get("filekey")
		if f.uploads == nil {
			f.uploads = map[string][]byte{}
		}
		f.uploads[ref] = raw
		f.mu.Unlock()
		w.Header().Set("x-encrypted-param", ref)
		w.WriteHeader(http.StatusOK)
		return
	}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(out)
}

var msgSeq struct {
	sync.Mutex
	n int
}

// say queues a message from `from` (with items, text when none are given).
func (f *fakeILink) say(from, text string, items ...map[string]any) {
	msgSeq.Lock()
	msgSeq.n++
	id := msgSeq.n
	msgSeq.Unlock()
	if len(items) == 0 {
		items = []map[string]any{{"type": 1, "text_item": map[string]any{"text": text}}}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, map[string]any{"message_id": id, "from_user_id": from, "context_token": "ctx-" + strconv.Itoa(id),
		"message_type": 1, "item_list": items})
}

// sentPictures are the image_items the bot sent, with the picture each names decrypted by the key it sent.
func (f *fakeILink) sentPictures(t *testing.T) ([]sentPicture, [][]byte) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var plain [][]byte
	for _, p := range f.pictures {
		keyHex, err := base64.StdEncoding.DecodeString(p.AESKey)
		if err != nil {
			t.Fatalf("aes_key %q: %v", p.AESKey, err)
		}
		key, err := hex.DecodeString(string(keyHex))
		if err != nil || len(key) != 16 {
			t.Fatalf("aes_key is not base64 of a hex key: %q", keyHex)
		}
		if p.ItemKey != string(keyHex) { // a receiver reads an image's key from its aeskey, as message.go does
			t.Fatalf("image_item aeskey %q, want the hex key %q", p.ItemKey, keyHex)
		}
		data, err := ilink.DecryptECB(f.uploads[p.Query], key)
		if err != nil {
			t.Fatalf("picture %s: %v", p.Query, err)
		}
		plain = append(plain, data)
	}
	return append([]sentPicture(nil), f.pictures...), plain
}

func (f *fakeILink) texts() []sentText {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentText(nil), f.sent...)
}

// fakeHub is the hub as the program reaches it (channel.receive): what it was handed.
type fakeHub struct {
	mu   sync.Mutex
	msgs []wire.ChannelMessage
}

func (h *fakeHub) Receive(_ context.Context, m wire.ChannelMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, m)
	return nil
}

func (h *fakeHub) got() []wire.ChannelMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]wire.ChannelMessage(nil), h.msgs...)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

type rig struct {
	t       *testing.T
	ilink   *fakeILink
	hub     *fakeHub
	secrets *memSecrets
	clock   *clock
	bot     *Bot
	cancel  context.CancelFunc
}

const owner = "owner@im.wechat"

// newRig is a bot bound to owner's WeChat, running against the fakes.
func newRig(t *testing.T, bound bool) *rig {
	r := &rig{t: t, ilink: newFakeILink(t), hub: &fakeHub{}, secrets: &memSecrets{}, clock: &clock{t: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}}
	if bound {
		st := state{Creds: &ilink.Credentials{Token: "BT", BotID: "bot@im.bot", UserID: owner, BaseURL: "https://ilinkai.weixin.qq.com/"}}
		data, _ := json.Marshal(st)
		r.secrets.data = data
	}
	r.bot = New(Options{Hub: r.hub, Agent: "Persona/test", Now: r.clock.now, DataDir: t.TempDir(),
		NewClient: func(base, token string) (*ilink.Client, error) {
			return ilink.New(ilink.Options{BaseURL: base, Token: token, Agent: "Persona/test", HTTP: &http.Client{Transport: r.ilink}})
		}}, r.secrets)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	go r.bot.Run(ctx)
	return r
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	writer, flags := log.Writer(), log.Flags()
	log.SetOutput(&out)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	return &out
}

func sendLogLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.HasPrefix(line, "wechat: send ") {
			out = append(out, line)
		}
	}
	return out
}

// sentTo waits for the bot to have sent a text to peer containing want; it returns that text.
func (r *rig) sentTo(peer, want string) sentText {
	r.t.Helper()
	var got sentText
	eventually(r.t, fmt.Sprintf("a message to %s with %q (sent: %+v)", peer, want, r.ilink.texts()), func() bool {
		for _, s := range r.ilink.texts() {
			if s.To == peer && strings.Contains(s.Text, want) {
				got = s
				return true
			}
		}
		return false
	})
	return got
}

func TestBindByQRCode(t *testing.T) {
	r := newRig(t, false)
	ctx := context.Background()
	eventually(t, "not signed in", func() bool { return r.bot.Status(ctx).State == wire.ConnectorStateNotSignedIn })
	r.ilink.statuses = []map[string]any{{"status": "scaned"}, {"status": "need_verifycode"},
		{"status": "confirmed", "bot_token": "BT", "ilink_bot_id": "bot@im.bot", "ilink_user_id": owner, "baseurl": "https://ilinkai.weixin.qq.com"}}
	p, err := r.bot.LoginStep(ctx, "", nil)
	if err != nil || p.Kind != wire.ConnectorPromptKindQr || p.URL != "https://liteapp.weixin.qq.com/q/example" || p.Interval == 0 {
		t.Fatalf("%+v %v", p, err)
	}
	if p, _ = r.bot.LoginStep(ctx, "qr", nil); !strings.Contains(p.Hint, "已经扫码") {
		t.Fatalf("scanned %+v", p)
	}
	if p, _ = r.bot.LoginStep(ctx, "qr", nil); p.Step != "code" || len(p.Fields) != 1 {
		t.Fatalf("code %+v", p)
	}
	if p, _ = r.bot.LoginStep(ctx, "code", map[string]string{"code": "x1"}); p.Error == "" {
		t.Fatalf("a bad code %+v", p)
	}
	if p, _ = r.bot.LoginStep(ctx, "code", map[string]string{"code": "123456"}); !p.Done || !strings.Contains(p.Account, "微信用户") {
		t.Fatalf("done %+v", p)
	}
	if st := r.secrets.state(t); st.Creds == nil || st.Creds.Token != "BT" || st.Creds.UserID != owner {
		t.Fatalf("stored %+v", st)
	}
	eventually(t, "serving", func() bool { return r.bot.Status(ctx).State == wire.ConnectorStateOk })
	if err := r.bot.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	st := r.secrets.state(t)
	if st.Creds != nil || len(st.Old) != 1 || st.Old[0] != "BT" {
		t.Fatalf("after logout %+v", st)
	}
	if s := r.bot.Status(ctx); s.State != wire.ConnectorStateNotSignedIn {
		t.Fatalf("status %+v", s)
	}
}

// International WeChat accounts redirect from ilinkai.weixin.qq.com to ilinkai.wechat.com during QR polling and
// confirm with https://ilinkai.wechat.com/ as their base URL.
func TestBindInternationalWeChatByQRCode(t *testing.T) {
	r := newRig(t, false)
	ctx := context.Background()
	eventually(t, "not signed in", func() bool { return r.bot.Status(ctx).State == wire.ConnectorStateNotSignedIn })
	r.ilink.statuses = []map[string]any{
		{"status": "scaned_but_redirect", "redirect_host": "ilinkai.wechat.com"},
		{"status": "confirmed", "bot_token": "BT-INTL", "ilink_bot_id": "bot@im.bot", "ilink_user_id": owner, "baseurl": "https://ilinkai.wechat.com/"},
	}
	p, err := r.bot.LoginStep(ctx, "", nil)
	if err != nil || p.Kind != wire.ConnectorPromptKindQr {
		t.Fatalf("begin: %+v %v", p, err)
	}
	if p, err = r.bot.LoginStep(ctx, "qr", nil); err != nil || p.Kind != wire.ConnectorPromptKindQr {
		t.Fatalf("redirect: %+v %v", p, err)
	}
	if p, err = r.bot.LoginStep(ctx, "qr", nil); err != nil || !p.Done {
		t.Fatalf("confirmed: %+v %v", p, err)
	}
	if st := r.secrets.state(t); st.Creds == nil || st.Creds.BaseURL != "https://ilinkai.wechat.com/" || st.Creds.Token != "BT-INTL" {
		t.Fatalf("stored %+v", st)
	}
	eventually(t, "serving on ilinkai.wechat.com", func() bool {
		if r.bot.Status(ctx).State != wire.ConnectorStateOk {
			return false
		}
		r.ilink.mu.Lock()
		defer r.ilink.mu.Unlock()
		return len(r.ilink.hosts) >= 4 && slices.Contains(r.ilink.hosts[3:], "ilinkai.wechat.com")
	})
	r.ilink.mu.Lock()
	hosts := append([]string(nil), r.ilink.hosts...)
	r.ilink.mu.Unlock()
	if hosts[0] != "ilinkai.weixin.qq.com" || hosts[1] != "ilinkai.weixin.qq.com" || hosts[2] != "ilinkai.wechat.com" {
		t.Fatalf("unexpected request hosts: %v", hosts)
	}
}

// A poll that fails for good (here iLink confirms with a base URL outside weixin.qq.com / wechat.com) must not leave
// the dead QR in place: asking again with the same code would fail the same way forever.
func TestFailedPollForgetsThePendingQR(t *testing.T) {
	r := newRig(t, false)
	ctx := context.Background()
	r.ilink.statuses = []map[string]any{{"status": "confirmed", "bot_token": "BT", "ilink_bot_id": "bot@im.bot", "ilink_user_id": owner, "baseurl": "https://ilink.example.com"}}
	if _, err := r.bot.LoginStep(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	_, err := r.bot.LoginStep(ctx, "qr", nil)
	if err == nil || ilink.Retryable(err) {
		t.Fatalf("want a final error, got %v", err)
	}
	if !strings.Contains(err.Error(), "source=baseurl") || !strings.Contains(err.Error(), `host="ilink.example.com"`) ||
		strings.Contains(err.Error(), "BT") || strings.Contains(err.Error(), owner) {
		t.Fatalf("unexpected final error diagnostic: %v", err)
	}
	r.bot.mu.Lock()
	pending := r.bot.qr
	r.bot.mu.Unlock()
	if pending != nil {
		t.Fatal("the QR that can no longer succeed is still pending")
	}
	if p, err := r.bot.LoginStep(ctx, "qr", nil); err != nil || p.Kind != wire.ConnectorPromptKindQr {
		t.Fatalf("a new QR should start, got %+v %v", p, err)
	}
	if st := r.secrets.state(t); st.Creds != nil {
		t.Fatalf("credentials from the rejected answer were stored: %+v", st)
	}
}

// The owner's message goes to the hub as it is; someone else hears once whose bot it is, and nothing reaches the hub.
func TestOwnerMessageGoesToTheHubStrangerHearsOnce(t *testing.T) {
	r := newRig(t, true)
	r.ilink.say("someone@im.wechat", "你好")
	r.sentTo("someone@im.wechat", "只为绑定它的人服务")
	r.ilink.say("someone@im.wechat", "在吗")
	r.ilink.say(owner, "明天有什么会？")
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) == 1 })
	m := r.hub.got()[0]
	if m.Text != "明天有什么会？" || !strings.HasPrefix(m.ID, "message:") {
		t.Fatalf("handed %+v", m)
	}
	n := 0
	for _, s := range r.ilink.texts() {
		if s.To == "someone@im.wechat" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("stranger heard %d times", n)
	}
	if st := r.secrets.state(t); st.Cursor == "" || st.Context == "" {
		t.Fatalf("cursor and context not kept: %+v", st)
	}
}

func TestTokenReplacedUnbinds(t *testing.T) {
	r := newRig(t, true)
	ctx := context.Background()
	eventually(t, "serving", func() bool { return r.bot.Status(ctx).State == wire.ConnectorStateOk })
	r.ilink.mu.Lock()
	r.ilink.replaced = true
	r.ilink.mu.Unlock()
	eventually(t, "unbound", func() bool {
		s := r.bot.Status(ctx)
		return s.State == wire.ConnectorStateNotSignedIn && strings.Contains(s.Error, "失效")
	})
	if st := r.secrets.state(t); st.Creds != nil || len(st.Old) != 1 {
		t.Fatalf("%+v", st)
	}
}

// A picture, documents and a video go with the message, all downloaded and decrypted. Videos are files to the hub.
func TestMediaGoesWithTheMessage(t *testing.T) {
	r := newRig(t, true)
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	pdf := append([]byte("%PDF-1.7\n"), []byte("synthetic PDF bytes")...)
	word := []byte("synthetic Word document bytes")
	video := append([]byte{0, 0, 0, 24}, []byte("ftypmp42synthetic video bytes")...)
	downloads := map[string][]byte{}
	for query, plain := range map[string][]byte{"I": png, "F": pdf, "W": word, "V": video} {
		enc, err := ilink.EncryptECB(plain, key)
		if err != nil {
			t.Fatal(err)
		}
		downloads[query] = enc
	}
	r.ilink.mu.Lock()
	r.ilink.downloads = downloads
	r.ilink.mu.Unlock()
	key64 := base64.StdEncoding.EncodeToString(key)
	r.ilink.say(owner, "",
		map[string]any{"type": 2, "image_item": map[string]any{"aeskey": hex.EncodeToString(key),
			"media": map[string]any{"encrypt_query_param": "I"}}},
		map[string]any{"type": 4, "file_item": map[string]any{"file_name": "report.pdf", "len": strconv.Itoa(len(pdf)),
			"media": map[string]any{"encrypt_query_param": "F", "aes_key": key64}}},
		map[string]any{"type": 4, "file_item": map[string]any{"file_name": "notes.doc",
			"media": map[string]any{"encrypt_query_param": "W", "aes_key": key64}}},
		map[string]any{"type": 5, "video_item": map[string]any{
			"media": map[string]any{"encrypt_query_param": "V", "aes_key": key64}}})
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) == 1 })
	m := r.hub.got()[0]
	if len(m.Images) != 1 || m.Images[0].Mime != "image/png" || m.Images[0].Data != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("image handed %+v", m.Images)
	}
	if len(m.Files) != 3 || len(m.Unsupported) != 0 {
		t.Fatalf("handed files=%+v unsupported=%v", m.Files, m.Unsupported)
	}
	for i, want := range []struct {
		name, mime string
		plain      []byte
	}{{"report.pdf", "application/pdf", pdf}, {"notes.doc", "application/msword", word}, {"video.mp4", "video/mp4", video}} {
		got := m.Files[i]
		if got.Name != want.name || got.Mime != want.mime || got.Data != base64.StdEncoding.EncodeToString(want.plain) {
			t.Errorf("file %d handed %+v, want name=%q mime=%q plaintext", i, got, want.name, want.mime)
		}
	}
}

// One failed download does not hide the other attachments; a wholly failed file still reaches the hub as unsupported.
func TestFailedMediaDownloadKeepsTheRest(t *testing.T) {
	r := newRig(t, true)
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	pdf := []byte("%PDF-1.7\nsynthetic PDF bytes")
	enc, err := ilink.EncryptECB(pdf, key)
	if err != nil {
		t.Fatal(err)
	}
	r.ilink.mu.Lock()
	r.ilink.downloads = map[string][]byte{"F": enc}
	r.ilink.mu.Unlock()
	key64 := base64.StdEncoding.EncodeToString(key)
	r.ilink.say(owner, "",
		map[string]any{"type": 4, "file_item": map[string]any{"file_name": "report.pdf",
			"media": map[string]any{"encrypt_query_param": "F", "aes_key": key64}}},
		map[string]any{"type": 5, "video_item": map[string]any{
			"media": map[string]any{"encrypt_query_param": "missing", "aes_key": key64}}})
	eventually(t, "the mixed message at the hub", func() bool { return len(r.hub.got()) == 1 })
	m := r.hub.got()[0]
	if len(m.Files) != 1 || m.Files[0].Name != "report.pdf" || len(m.Unsupported) != 1 || m.Unsupported[0] != "一段视频" {
		t.Fatalf("mixed message handed %+v", m)
	}

	r.ilink.say(owner, "", map[string]any{"type": 4, "file_item": map[string]any{"file_name": "broken.pdf",
		"media": map[string]any{"encrypt_query_param": "broken"}}})
	eventually(t, "the failed file at the hub", func() bool { return len(r.hub.got()) == 2 })
	m = r.hub.got()[1]
	if len(m.Files) != 0 || len(m.Unsupported) != 1 || m.Unsupported[0] != "文件「broken.pdf」" {
		t.Fatalf("failed file handed %+v", m)
	}
}

// The raw attachment budget applies before Base64 reaches the process RPC. Unknown sizes are checked after download;
// a declared oversize attachment is skipped without downloading it.
func TestMessageMediaBudget(t *testing.T) {
	f := newFakeILink(t)
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	plain := make([]byte, 8<<20)
	copy(plain, "\x89PNG\r\n\x1a\n")
	enc, err := ilink.EncryptECB(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.downloads = map[string][]byte{"F": enc}
	f.mu.Unlock()
	c, err := ilink.New(ilink.Options{BaseURL: "https://ilinkai.weixin.qq.com", Token: "BT", Agent: "Persona/test",
		HTTP: &http.Client{Transport: f}})
	if err != nil {
		t.Fatal(err)
	}
	key64 := base64.StdEncoding.EncodeToString(key)
	media := make([]ilink.Media, 8)
	media[0] = ilink.Media{Kind: ilink.MediaImage, Query: "F", KeyHex: hex.EncodeToString(key)}
	for i := 0; i < 7; i++ {
		media[i+1] = ilink.Media{Kind: ilink.MediaFile, Query: "F", KeyB64: key64,
			FileName: fmt.Sprintf("%d.pdf", i), Size: int64(len(plain))}
	}
	media[5].Size = 0 // the fifth file is still downloaded, then rejected by the decrypted-size guard
	got := (&Bot{}).compose(context.Background(), c, owner, ilink.Message{Media: media})
	if len(got.Images) != 1 || len(got.Files) != 4 || len(got.Unsupported) != 3 ||
		got.Unsupported[0] != "文件「4.pdf」" || got.Unsupported[1] != "文件「5.pdf」" || got.Unsupported[2] != "文件「6.pdf」" {
		t.Fatalf("budget handed %d images, %d files and %d unsupported, want 1, 4 and 3", len(got.Images), len(got.Files), len(got.Unsupported))
	}
}

func TestDuplicateMessageHandledOnce(t *testing.T) {
	r := newRig(t, true)
	item := []map[string]any{{"type": 1, "text_item": map[string]any{"text": "一次就好"}}}
	r.ilink.mu.Lock()
	for range 2 {
		r.ilink.queue = append(r.ilink.queue, map[string]any{"message_id": 9001, "from_user_id": owner, "context_token": "c", "message_type": 1, "item_list": item})
	}
	r.ilink.mu.Unlock()
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if n := len(r.hub.got()); n != 1 {
		t.Fatalf("handed %d times", n)
	}
}

// channel.send: nothing to send with before the owner's first message (cannot_send); then WeChat's Markdown, in the
// chat of their latest message.
func TestSendNeedsTheOwnersChat(t *testing.T) {
	r := newRig(t, true)
	ctx := context.Background()
	eventually(t, "serving", func() bool { return r.bot.Status(ctx).State == wire.ConnectorStateOk })
	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "a", Text: "早"}); !errors.Is(err, errCannotSend) {
		t.Fatalf("before the first message: %v", err)
	}
	r.ilink.say(owner, "在吗")
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) == 1 })
	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "b", Text: "**明天** 10 点有周会。![图](x.png)", Kind: wire.ChannelSendKindAnswer}); err != nil {
		t.Fatal(err)
	}
	got := r.sentTo(owner, "10 点有周会")
	if got.Context == "" || !strings.HasPrefix(got.Text, "**明天**") || strings.Contains(got.Text, "x.png") { // WeChat shows bold, not pictures
		t.Fatalf("sent %+v", got)
	}
}

// Every piece gets one final send log with its inbound message, hub ref, result and duration; the text stays out.
func TestSendLogsEachPiece(t *testing.T) {
	wait := sendRetryWait
	sendRetryWait = time.Millisecond
	t.Cleanup(func() { sendRetryWait = wait })
	r := newRig(t, true)
	ctx := context.Background()
	r.ilink.say(owner, "在吗")
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) == 1 })
	eventually(t, "serving", func() bool { return r.bot.Status(ctx).State == wire.ConnectorStateOk })
	logs := captureLog(t)

	p := wire.ChannelSendParams{Ref: "run-1:4", ReplyTo: "message:42", Text: strings.Repeat("answer-body-", 300),
		Kind: wire.ChannelSendKindAnswer}
	if _, err := r.bot.Send(ctx, p); err != nil {
		t.Fatal(err)
	}
	lines := sendLogLines(logs.String())
	if len(lines) != 2 {
		t.Fatalf("got %d send logs, want one for each of two pieces: %q", len(lines), logs.String())
	}
	for i, line := range lines {
		for _, want := range []string{`reply_to="message:42"`, `ref="run-1:4"`, "part=" + strconv.Itoa(i+1) + "/2",
			"result=success", "duration="} {
			if !strings.Contains(line, want) {
				t.Errorf("piece %d log %q does not contain %q", i+1, line, want)
			}
		}
	}
	if strings.Contains(logs.String(), "answer-body-") {
		t.Fatal("send logs contain the answer text")
	}

	logs.Reset()
	r.ilink.mu.Lock()
	r.ilink.failSends = 3
	r.ilink.mu.Unlock()
	p = wire.ChannelSendParams{Ref: "run-2:5", ReplyTo: "message:43", Text: "not-delivered-body",
		Kind: wire.ChannelSendKindAnswer}
	if _, err := r.bot.Send(ctx, p); err == nil {
		t.Fatal("the send should fail after all three attempts")
	}
	lines = sendLogLines(logs.String())
	if len(lines) != 1 {
		t.Fatalf("got %d failure logs, want one: %q", len(lines), logs.String())
	}
	for _, want := range []string{`reply_to="message:43"`, `ref="run-2:5"`, "part=1/1", "result=failure",
		"duration=", "error_type=http", "error=iLink answered HTTP 500"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("failure log %q does not contain %q", lines[0], want)
		}
	}
	if strings.Contains(logs.String(), p.Text) {
		t.Fatal("failure log contains the answer text")
	}
}

// Cancelling the wait after a retryable response is still a final failure and must not skip the piece's log.
func TestSendLogsCancellationDuringRetry(t *testing.T) {
	wait := sendRetryWait
	sendRetryWait = time.Hour
	t.Cleanup(func() { sendRetryWait = wait })
	r := newRig(t, true)
	r.ilink.say(owner, "在吗")
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) == 1 })
	r.ilink.mu.Lock()
	r.ilink.failSends = 1
	r.ilink.failedSend = make(chan struct{}, 1)
	r.ilink.mu.Unlock()
	logs := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := wire.ChannelSendParams{Ref: "run-3:6", ReplyTo: "message:44", Text: "cancelled", Kind: wire.ChannelSendKindAnswer}
	done := make(chan error, 1)
	go func() {
		_, err := r.bot.Send(ctx, p)
		done <- err
	}()
	select {
	case <-r.ilink.failedSend:
	case <-time.After(5 * time.Second):
		t.Fatal("the first retryable send did not finish")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled send should fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the retry wait did not stop the send")
	}
	lines := sendLogLines(logs.String())
	if len(lines) != 1 {
		t.Fatalf("got %d cancellation logs, want one: %q", len(lines), logs.String())
	}
	for _, want := range []string{`reply_to="message:44"`, `ref="run-3:6"`, "part=1/1", "result=failure",
		"duration=", "error_type=aborted"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("cancellation log %q does not contain %q", lines[0], want)
		}
	}
}

// A send whose answer is lost is sent again with the same client_id, by the bot and by the hub's retry of the same
// ref after the bot gave up: iLink keeps it once (#4530).
func TestSendRetryKeepsTheClientID(t *testing.T) {
	wait := sendRetryWait
	sendRetryWait = time.Millisecond
	t.Cleanup(func() { sendRetryWait = wait })
	r := newRig(t, true)
	ctx := context.Background()
	r.ilink.say(owner, "在吗")
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) == 1 })
	eventually(t, "serving", func() bool { return r.bot.Status(ctx).State == wire.ConnectorStateOk })
	r.ilink.mu.Lock()
	r.ilink.failSends, r.ilink.clientIDs, r.ilink.sent = 3, nil, nil // every attempt of the first Send loses its answer
	r.ilink.mu.Unlock()
	p := wire.ChannelSendParams{Ref: "run-1:4", Text: "答案", Kind: wire.ChannelSendKindAnswer}
	if _, err := r.bot.Send(ctx, p); err == nil {
		t.Fatal("the first Send should fail: no attempt got an answer")
	}
	if _, err := r.bot.Send(ctx, p); err != nil { // the hub sends the same ref again
		t.Fatal(err)
	}
	r.ilink.mu.Lock()
	ids, sent := append([]string(nil), r.ilink.clientIDs...), append([]sentText(nil), r.ilink.sent...)
	r.ilink.mu.Unlock()
	if len(ids) != 4 || len(slices.Compact(slices.Clone(ids))) != 1 {
		t.Fatalf("client_ids %q: every attempt must carry the same one", ids)
	}
	if len(sent) != 1 {
		t.Fatalf("the owner got %d copies: %+v", len(sent), sent)
	}
	if newSendIDs("other", owner, 1, []string{p.Text}, nil).parts[0] == ids[0] || newSendIDs(p.Ref, owner, 1, []string{"另一个答案"}, nil).parts[0] == ids[0] ||
		newSendIDs(p.Ref, "other@im.wechat", 1, []string{p.Text}, nil).parts[0] == ids[0] ||
		newSendIDs(p.Ref, owner, 2, []string{p.Text}, nil).parts[0] == ids[0] {
		t.Fatal("another ref, text, WeChat or round must get another client_id")
	}
}

// "typing…" while the hub says so, off when it says it is done.
func TestTypingWhileTheHubSays(t *testing.T) {
	r := newRig(t, true)
	ctx := context.Background()
	r.ilink.say(owner, "在吗")
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) == 1 })
	if err := r.bot.Typing(ctx, true); err != nil {
		t.Fatal(err)
	}
	_ = r.bot.Typing(ctx, true) // again within 5 s: WeChat is not told twice
	if err := r.bot.Typing(ctx, false); err != nil {
		t.Fatal(err)
	}
	r.ilink.mu.Lock()
	typing := append([]bool(nil), r.ilink.typing...)
	r.ilink.mu.Unlock()
	if len(typing) != 2 || !typing[0] || typing[1] {
		t.Fatalf("typing %v", typing)
	}
}

// What the bot writes for people reads in the language the hub said at its start; Chinese when it said none.
func TestTextInTheUsersLanguage(t *testing.T) {
	if got := New(Options{Lang: langEn}, nil).status.Error; got != "WeChat isn't bound yet" {
		t.Fatal(got)
	}
	if got := New(Options{}, nil).status.Error; got != "还没绑定微信" {
		t.Fatal(got)
	}
	if p := codePrompt(langJa, ""); p.Fields[0].Label != "確認コード" {
		t.Fatal(p.Fields[0].Label)
	}
	c := &ilink.Credentials{UserID: "owner-fixture@example.invalid"}
	if got := account(langEn, c); got != "WeChat user ixture" {
		t.Fatal(got)
	}
}

// channel.send with pictures (images_out): the text first, then each picture encrypted with a fresh key, uploaded to
// the CDN where getuploadurl says and sent as an image_item naming it; an upload that fails is tried again; the same
// ref again posts nothing twice; pictures alone go without text.
func TestSendSendsPicturesAfterTheText(t *testing.T) {
	wait := sendRetryWait
	sendRetryWait = time.Millisecond
	t.Cleanup(func() { sendRetryWait = wait })
	r := newRig(t, true)
	ctx := context.Background()
	r.ilink.say(owner, "在吗")
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) == 1 })
	eventually(t, "serving", func() bool { return r.bot.Status(ctx).State == wire.ConnectorStateOk })
	pic1 := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 100)...)
	pic2 := []byte("\xff\xd8\xff a second picture")
	b64 := base64.StdEncoding.EncodeToString
	r.ilink.mu.Lock()
	r.ilink.failUploads = 1
	r.ilink.mu.Unlock()
	p := wire.ChannelSendParams{Ref: "run-7:3", Text: "图在这里", Kind: wire.ChannelSendKindAnswer,
		Images: []wire.ChannelBlob{{Mime: "image/png", Data: b64(pic1)}, {Mime: "image/jpeg", Data: b64(pic2)}}}
	if _, err := r.bot.Send(ctx, p); err != nil {
		t.Fatal(err)
	}
	r.sentTo(owner, "图在这里")
	pics, plain := r.ilink.sentPictures(t)
	if len(pics) != 2 || !bytes.Equal(plain[0], pic1) || !bytes.Equal(plain[1], pic2) {
		t.Fatalf("sent %d pictures", len(pics))
	}
	r.ilink.mu.Lock()
	reqs := slices.Clone(r.ilink.uploadReq)
	for _, pic := range pics {
		if pic.To != owner || pic.Context == "" || pic.EncryptType != 1 || int(pic.MidSize) != len(r.ilink.uploads[pic.Query]) {
			t.Errorf("picture %+v", pic)
		}
	}
	r.ilink.mu.Unlock()
	if len(reqs) != 3 { // the first picture's failed upload is tried again with a new upload URL
		t.Fatalf("%d getuploadurl calls", len(reqs))
	}
	for i, req := range reqs {
		src := [][]byte{pic1, pic1, pic2}[i]
		sum := md5.Sum(src)
		if req["media_type"] != float64(1) || req["to_user_id"] != owner || req["rawsize"] != float64(len(src)) ||
			req["rawfilemd5"] != hex.EncodeToString(sum[:]) || req["filesize"] != float64((len(src)/16+1)*16) ||
			req["no_need_thumb"] != true || len(req["aeskey"].(string)) != 32 || len(req["filekey"].(string)) != 32 {
			t.Errorf("getuploadurl %v", req)
		}
	}

	if _, err := r.bot.Send(ctx, p); err != nil { // the hub sends the same ref again: iLink keeps each once
		t.Fatal(err)
	}
	if pics, _ := r.ilink.sentPictures(t); len(pics) != 2 {
		t.Fatalf("the same ref again posted %d pictures", len(pics))
	}

	alone := wire.ChannelSendParams{Ref: "run-8:1", Kind: wire.ChannelSendKindAnswer, Images: []wire.ChannelBlob{{Mime: "image/png", Data: b64(pic1)}}}
	texts := len(r.ilink.texts())
	if _, err := r.bot.Send(ctx, alone); err != nil {
		t.Fatal(err)
	}
	if pics, _ := r.ilink.sentPictures(t); len(pics) != 3 || len(r.ilink.texts()) != texts {
		t.Fatalf("pictures alone: %d pictures, %d texts", len(pics), len(r.ilink.texts())-texts)
	}
}

// A picture that can't be sent at all (not base64) is skipped; one whose upload keeps failing in a way that may pass
// (HTTP 500) fails the send, which the hub tries again with the same ref.
func TestSendPictureFailures(t *testing.T) {
	wait := sendRetryWait
	sendRetryWait = time.Millisecond
	t.Cleanup(func() { sendRetryWait = wait })
	r := newRig(t, true)
	ctx := context.Background()
	r.ilink.say(owner, "在吗")
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) == 1 })
	eventually(t, "serving", func() bool { return r.bot.Status(ctx).State == wire.ConnectorStateOk })
	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "a", Text: "坏图", Kind: wire.ChannelSendKindAnswer,
		Images: []wire.ChannelBlob{{Mime: "image/png", Data: "not base64!"}}}); err != nil {
		t.Fatalf("an undecodable picture: %v", err)
	}
	r.ilink.mu.Lock()
	r.ilink.failUploads = 100
	r.ilink.mu.Unlock()
	_, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "b", Kind: wire.ChannelSendKindAnswer,
		Images: []wire.ChannelBlob{{Mime: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("png"))}}})
	var failed sendFailed
	if !errors.As(err, &failed) {
		t.Fatalf("a refused upload: %v", err)
	}
	if pics, _ := r.ilink.sentPictures(t); len(pics) != 0 {
		t.Fatalf("sent %d pictures", len(pics))
	}
	r.ilink.mu.Lock()
	tries := 100 - r.ilink.failUploads
	r.ilink.mu.Unlock()
	if tries != 3 { // three tries in all, each after a wait: no burst of uploads within a try
		t.Fatalf("the CDN got %d uploads of a refused picture, want 3", tries)
	}
}

// iLink's calls so far: every sendmessage (repeated client_ids too), getuploadurl, and CDN upload the CDN took.
func (f *fakeILink) calls() (sends, uploadURLs, uploads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.clientIDs), len(f.uploadReq), len(f.uploads)
}

// pictureSendRig is a bound, serving bot with the fast retries of the picture tests.
func pictureSendRig(t *testing.T) *rig {
	t.Helper()
	wait := sendRetryWait
	sendRetryWait = time.Millisecond
	t.Cleanup(func() { sendRetryWait = wait })
	r := newRig(t, true)
	r.ilink.say(owner, "在吗")
	eventually(t, "the message at the hub", func() bool { return len(r.hub.got()) == 1 })
	eventually(t, "serving", func() bool { return r.bot.Status(context.Background()).State == wire.ConnectorStateOk })
	return r
}

func (r *rig) progressOf(ref string) (sendProgress, bool) {
	r.bot.mu.Lock()
	defer r.bot.mu.Unlock()
	if pr, ok := r.bot.progress[ref]; ok {
		return pr, true
	}
	return sendProgress{}, false
}

var testPicture = func(n byte) wire.ChannelBlob {
	data := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{n}, 64)...)
	return wire.ChannelBlob{Mime: "image/png", Data: base64.StdEncoding.EncodeToString(data)}
}

// A send of text and two pictures whose second picture is refused fails; the same ref sent again goes on from that
// picture: the text and the first picture are not sent, uploaded or asked an upload URL for again. Counted from what
// iLink was asked, not from its keeping a client_id once.
func runResumeScenario(t *testing.T, r *rig, ref string, send func(wire.ChannelSendParams) error) {
	t.Helper()
	p := wire.ChannelSendParams{Ref: ref, ReplyTo: "message:7", Text: "看图", Kind: wire.ChannelSendKindAnswer,
		Images: []wire.ChannelBlob{testPicture(1), testPicture(2)}}
	r.ilink.mu.Lock()
	r.ilink.okUploads, r.ilink.failUploads = 1, 3 // the first picture goes; the second is refused on all three tries
	r.ilink.mu.Unlock()
	if err := send(p); err == nil {
		t.Fatal("the first send should fail: the second picture is refused")
	}
	sends, urls, ups := r.ilink.calls()
	if sends != 2 || urls != 4 || ups != 1 { // the text and picture 1 sent; picture 1 and picture 2's three tries asked
		t.Fatalf("after the failure: %d sendmessage, %d getuploadurl, %d uploads", sends, urls, ups)
	}
	ids := newSendIDs(ref, owner, 1, splitText(p.Text), p.Images)
	if pr, ok := r.progressOf(ref); !ok || pr.sum != ids.sum || len(pr.done) != 2 || !pr.done[ids.parts[0]] || !pr.done[ids.images[0]] ||
		len(pr.skipped) != 0 || pr.uploadOf != ids.images[1] || pr.upload.Query != "" {
		t.Fatalf("progress after the failure: %+v %v", pr, ok)
	}
	logs := captureLog(t)
	if err := send(p); err != nil { // the hub sends the same ref again
		t.Fatal(err)
	}
	sends2, urls2, ups2 := r.ilink.calls()
	if sends2-sends != 1 || urls2-urls != 1 || ups2-ups != 1 { // only picture 2
		t.Fatalf("the retry made %d sendmessage, %d getuploadurl, %d uploads calls, want one of each (picture 2)",
			sends2-sends, urls2-urls, ups2-ups)
	}
	if pics, _ := r.ilink.sentPictures(t); len(pics) != 2 || len(r.ilink.texts()) != 1 {
		t.Fatalf("iLink holds %d pictures and %d texts", len(pics), len(r.ilink.texts()))
	}
	if _, ok := r.progressOf(ref); ok {
		t.Fatal("progress is kept after the send went through")
	}
	if !strings.Contains(logs.String(), `ref="`+ref+`" resume text=1/1 image=1/2`) {
		t.Fatalf("no resume log: %q", logs.String())
	}
}

func TestSendResumesAtTheFailedPicture(t *testing.T) {
	r := pictureSendRig(t)
	runResumeScenario(t, r, "run-9:1", func(p wire.ChannelSendParams) error {
		_, err := r.bot.Send(context.Background(), p)
		return err
	})
}

// The same through the program's channel.send, where sentRefs lets a failed ref go again.
func TestProgramResumesAtTheFailedPicture(t *testing.T) {
	r := pictureSendRig(t)
	p := &program{bot: r.bot}
	runResumeScenario(t, r, "run-9:2", func(sp wire.ChannelSendParams) error {
		sp.Contribution = ID
		params, _ := json.Marshal(sp)
		_, err := p.channel(context.Background(), "send", params)
		return err
	})
}

// A ref sent again with other content (the hub fetches an answer's pictures again when it sends the answer again) is
// another message: it is sent again from the start, every piece under a new client_id, so iLink takes none of it for a
// repeat of what was sent before. The progress kept for the old content is dropped.
func TestSendOfChangedContent(t *testing.T) {
	r := pictureSendRig(t)
	ctx := context.Background()
	ref := "run-10:1"
	r.ilink.mu.Lock()
	r.ilink.okUploads, r.ilink.failUploads = 1, 3
	r.ilink.mu.Unlock()
	two := wire.ChannelSendParams{Ref: ref, Text: "看图", Kind: wire.ChannelSendKindAnswer, Images: []wire.ChannelBlob{testPicture(1), testPicture(2)}}
	if _, err := r.bot.Send(ctx, two); err == nil {
		t.Fatal("the first send should fail")
	}
	r.ilink.mu.Lock()
	first := slices.Clone(r.ilink.clientIDs)
	r.ilink.mu.Unlock()
	other := two
	other.Images = []wire.ChannelBlob{testPicture(1), testPicture(3)} // picture 2 replaced: as many pictures as before
	before := r.tally()
	if _, err := r.bot.Send(ctx, other); err != nil {
		t.Fatal(err)
	}
	if got := r.tally().minus(before); got != (tally{sends: 3, uploadURLs: 2, cdn: 2}) { // the text and both pictures again
		t.Fatalf("changed content: calls %+v, want all of it sent again", got)
	}
	r.ilink.mu.Lock()
	again := slices.Clone(r.ilink.clientIDs[len(first):])
	r.ilink.mu.Unlock()
	for _, id := range again {
		if slices.Contains(first, id) {
			t.Fatalf("client_id %s of the old content is used for the new one", id)
		}
	}
	_, plain := r.ilink.sentPictures(t)
	texts := r.ilink.texts()
	if len(plain) != 3 || !samePicture(plain[1], testPicture(1)) || !samePicture(plain[2], testPicture(3)) ||
		len(texts) != 2 || texts[1].Text != "看图" {
		t.Fatalf("iLink holds %d pictures and %d texts, want the new message whole after the old one's start", len(plain), len(texts))
	}
	if _, ok := r.progressOf(ref); ok {
		t.Fatal("progress is kept after the send went through")
	}

	// A new text, in fewer pieces: the new text and both pictures go.
	long := wire.ChannelSendParams{Ref: "run-10:2", Text: strings.Repeat("answer-body-", 300), Kind: wire.ChannelSendKindAnswer,
		Images: []wire.ChannelBlob{testPicture(4), testPicture(5)}}
	r.ilink.mu.Lock()
	r.ilink.okUploads, r.ilink.failUploads = 1, 3
	r.ilink.mu.Unlock()
	if _, err := r.bot.Send(ctx, long); err == nil {
		t.Fatal("the long send should fail")
	}
	if pr, ok := r.progressOf("run-10:2"); !ok || len(pr.done) != 3 {
		t.Fatalf("progress %+v %v: want two pieces and a picture", pr, ok)
	}
	short := long
	short.Text = "short"
	before = r.tally()
	if _, err := r.bot.Send(ctx, short); err != nil {
		t.Fatal(err)
	}
	if got := r.tally().minus(before); got.sends != 3 { // the new piece and both pictures
		t.Fatalf("changed text: %d sendmessage calls, want 3", got.sends)
	}
	if got := r.ilink.texts(); got[len(got)-1].Text != "short" {
		t.Fatalf("the new text was not sent: %+v", got[len(got)-1])
	}
}

// Progress is bounded: past progressMost refs the oldest is dropped (its send starts over); a ref kept again does not
// take another place. What is kept for other content under a ref is dropped.
func TestSendProgressKeepsTheLatestRefs(t *testing.T) {
	b := New(Options{}, &memSecrets{})
	ids := func(ref string) sendIDs {
		return newSendIDs(ref, owner, 1, []string{"text"}, []wire.ChannelBlob{testPicture(1)})
	}
	partDone := func(ref string) sendProgress {
		return sendProgress{sum: ids(ref).sum, round: 1, done: map[string]bool{ids(ref).parts[0]: true}}
	}
	for i := range progressMost + 1 {
		b.keep("ref-"+strconv.Itoa(i), 0, partDone("ref-"+strconv.Itoa(i)))
	}
	newest := "ref-" + strconv.Itoa(progressMost)
	all := partDone(newest)
	all.done[ids(newest).images[0]] = true
	b.keep(newest, 0, all) // kept again
	if len(b.progress) != progressMost || len(b.progressOrder) != progressMost {
		t.Fatalf("%d refs kept, %d in order, want %d", len(b.progress), len(b.progressOrder), progressMost)
	}
	if pr := b.resume("ref-0", 0, ids("ref-0")); len(pr.done) != 0 {
		t.Fatalf("the oldest ref is kept: %+v", pr)
	}
	if pr := b.resume("ref-1", 0, ids("ref-1")); len(pr.done) != 1 {
		t.Fatalf("the second oldest ref: %+v", pr)
	}
	if pr := b.resume(newest, 0, ids(newest)); len(pr.done) != 2 {
		t.Fatalf("the newest ref: %+v", pr)
	}
	if pr := b.resume("ref-2", 0, newSendIDs("ref-2", owner, 1, []string{"other text"}, nil)); len(pr.done) != 0 ||
		len(b.progress) != progressMost-1 || len(b.progressOrder) != progressMost-1 {
		t.Fatalf("progress of other content is kept: %+v, %d refs", pr, len(b.progress))
	}
	b.keep("", 0, partDone(""))
	if len(b.progress) != progressMost-1 {
		t.Fatal("progress kept without a ref")
	}
}

// Without a ref (the bot's own messages) nothing is kept, and a send that fails stays as it was.
func TestSendWithoutARefKeepsNoProgress(t *testing.T) {
	r := pictureSendRig(t)
	r.ilink.mu.Lock()
	r.ilink.okUploads, r.ilink.failUploads = 1, 3
	r.ilink.mu.Unlock()
	p := wire.ChannelSendParams{Text: "看图", Kind: wire.ChannelSendKindAnswer, Images: []wire.ChannelBlob{testPicture(1), testPicture(2)}}
	if _, err := r.bot.Send(context.Background(), p); err == nil {
		t.Fatal("the send should fail")
	}
	r.bot.mu.Lock()
	n := len(r.bot.progress)
	r.bot.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d refs kept for a send without a ref", n)
	}
}

// Each picture gets one final send log like a text piece's (image=i/n for part=i/n): success, failure with its error
// type, skipped with why, and cancelled; none holds the picture.
func TestSendLogsEachPicture(t *testing.T) {
	r := pictureSendRig(t)
	ctx := context.Background()
	logs := captureLog(t)
	pic := testPicture(9)
	big := wire.ChannelBlob{Mime: "image/png", Data: base64.StdEncoding.EncodeToString(make([]byte, ilink.MaxMedia+1))}
	send := func(ctx context.Context, ref, replyTo string, imgs ...wire.ChannelBlob) error {
		_, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: ref, ReplyTo: replyTo, Kind: wire.ChannelSendKindAnswer, Images: imgs})
		return err
	}
	lines := func() []string {
		var out []string
		for _, l := range sendLogLines(logs.String()) {
			if strings.Contains(l, " image=") {
				out = append(out, l)
			}
		}
		logs.Reset()
		return out
	}
	check := func(what string, got []string, wants ...[]string) {
		t.Helper()
		if len(got) != len(wants) {
			t.Fatalf("%s: %d picture logs, want %d: %q", what, len(got), len(wants), got)
		}
		for i, line := range got {
			for _, want := range wants[i] {
				if !strings.Contains(line, want) {
					t.Errorf("%s: log %q does not contain %q", what, line, want)
				}
			}
		}
	}

	if err := send(ctx, "run-l:1", "message:50", pic); err != nil {
		t.Fatal(err)
	}
	check("success", lines(), []string{`reply_to="message:50"`, `ref="run-l:1"`, "image=1/1", "result=success", "duration="})

	r.ilink.mu.Lock()
	r.ilink.failUploads = 3
	r.ilink.mu.Unlock()
	if err := send(ctx, "run-l:2", "message:51", pic); err == nil {
		t.Fatal("a refused picture should fail the send")
	}
	check("failure", lines(), []string{`reply_to="message:51"`, `ref="run-l:2"`, "image=1/1", "result=failure", "duration=",
		"error_type=http", "error=picture upload answered HTTP 500"})

	if err := send(ctx, "run-l:3", "message:52", wire.ChannelBlob{Data: "not base64!"}, wire.ChannelBlob{}, big, pic); err != nil {
		t.Fatal(err)
	}
	check("skipped", lines(),
		[]string{`ref="run-l:3"`, "image=1/4", "result=skipped", "reason=not_base64", "duration="},
		[]string{"image=2/4", "result=skipped", "reason=empty"},
		[]string{"image=3/4", "result=skipped", "reason=too_large"},
		[]string{"image=4/4", "result=success"})

	// Cancelled while waiting to try a refused upload again.
	sendRetryWait = time.Hour // pictureSendRig puts it back when the test ends
	r.ilink.mu.Lock()
	r.ilink.failUploads = 1
	r.ilink.mu.Unlock()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- send(cctx, "run-l:4", "message:53", pic) }()
	eventually(t, "the refused upload", func() bool {
		r.ilink.mu.Lock()
		defer r.ilink.mu.Unlock()
		return r.ilink.failUploads == 0
	})
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled send should fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling did not stop the send")
	}
	check("cancelled", lines(), []string{`reply_to="message:53"`, `ref="run-l:4"`, "image=1/1", "result=failure", "duration=", "error_type=aborted"})
	sendRetryWait = time.Millisecond

	// Nothing of a picture, its upload or its keys is logged.
	logs.Reset()
	r.ilink.mu.Lock()
	r.ilink.failUploads = 3
	r.ilink.mu.Unlock()
	_ = send(ctx, "run-l:5", "message:54", pic)
	_ = send(ctx, "run-l:6", "message:55", pic, wire.ChannelBlob{Data: "not base64!"})
	for _, secret := range []string{pic.Data, "https://", "encrypted_query_param", "filekey", "aes", "UP-"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain %q: %q", secret, logs.String())
		}
	}
}
