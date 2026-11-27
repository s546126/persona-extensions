// Package apitest is a fake DingTalk open platform for tests: the access token, Stream mode (a WebSocket the test
// pushes frames into), sending to one person, downloads and session webhooks. Everything in it is synthetic.
package apitest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The fake app's credentials.
const (
	ClientID     = "ding-synthetic-app"
	ClientSecret = "synthetic-secret"
	token        = "synthetic-access-token"
	ticket       = "synthetic-ticket"
)

// Sent is a message the robot sent to one person.
type Sent struct {
	StaffID string
	Title   string
	Text    string
}

// Fake is the fake open platform.
type Fake struct {
	Server *httptest.Server

	mu sync.Mutex
	// Files are what a download code downloads.
	Files map[string][]byte
	// Invalid and Held are the staff ids the robot cannot reach and those DingTalk holds back (flow control).
	Invalid, Held map[string]bool
	// Fail answers every send with this HTTP status (0: none).
	Fail int
	// LoseAnswers is how many more sends are taken (recorded as delivered) but answered by dropping the
	// connection: the client cannot tell whether they arrived.
	LoseAnswers int
	// Limit answers this many more sends with HTTP 429 before taking them.
	Limit int
	// RefuseAfter refuses every send (HTTP 400) once this many were delivered (0: never).
	RefuseAfter int
	// ExpireToken answers this many more sends with HTTP 401, as DingTalk does for a token it revoked early.
	ExpireToken int
	// EndpointHost, when set, is the host the Stream endpoint is given on.
	EndpointHost string
	// PlainDownloads gives download addresses as http (as DingTalk does for its OSS buckets).
	PlainDownloads bool
	tokens         int
	opens          int
	sent           []Sent
	replies        []string
	acks           []map[string]any
	conn           *websocket.Conn
	connCtx        context.Context
	up             chan struct{}
}

// New starts a fake that closes with the test.
func New(t testing.TB) *Fake {
	f := &Fake{Files: map[string][]byte{}, Invalid: map[string]bool{}, Held: map[string]bool{}, up: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1.0/oauth2/accessToken", f.accessToken)
	mux.HandleFunc("POST /v1.0/gateway/connections/open", f.open)
	mux.HandleFunc("GET /connect", f.connect)
	mux.HandleFunc("POST /v1.0/robot/oToMessages/batchSend", f.send)
	mux.HandleFunc("POST /v1.0/robot/messageFiles/download", f.download)
	mux.HandleFunc("GET /files/{code}", f.file)
	mux.HandleFunc("POST /webhook/{id}", f.webhook)
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

// URL is the fake's address (the client's BaseURL); Client is an HTTP client that trusts it.
func (f *Fake) URL() string          { return f.Server.URL }
func (f *Fake) Client() *http.Client { return f.Server.Client() }

// Webhook is a session webhook of the fake for a message.
func (f *Fake) Webhook(id string) string { return f.Server.URL + "/webhook/" + id }

func readJSON(r *http.Request, v any) bool {
	data, err := io.ReadAll(r.Body)
	return err == nil && json.Unmarshal(data, v) == nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalidClientSecret", "message": "the app's credentials are wrong"})
}

func (f *Fake) accessToken(w http.ResponseWriter, r *http.Request) {
	var in struct{ AppKey, AppSecret string }
	if !readJSON(r, &in) || in.AppKey != ClientID || in.AppSecret != ClientSecret {
		refuse(w)
		return
	}
	f.mu.Lock()
	f.tokens++
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"accessToken": token, "expireIn": 7200})
}

func (f *Fake) open(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID      string `json:"clientId"`
		ClientSecret  string `json:"clientSecret"`
		Subscriptions []struct{ Type, Topic string }
	}
	if !readJSON(r, &in) || in.ClientID != ClientID || in.ClientSecret != ClientSecret || len(in.Subscriptions) == 0 {
		refuse(w)
		return
	}
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	host := r.Host
	if f.EndpointHost != "" {
		host = f.EndpointHost
	}
	writeJSON(w, http.StatusOK, map[string]string{"endpoint": "wss://" + host + "/connect", "ticket": ticket})
}

func (f *Fake) connect(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("ticket") != ticket {
		http.Error(w, "bad ticket", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conn, f.connCtx = c, r.Context()
	f.mu.Unlock()
	f.up <- struct{}{}
	for {
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var a map[string]any
		if json.Unmarshal(data, &a) == nil {
			f.mu.Lock()
			f.acks = append(f.acks, a)
			f.mu.Unlock()
		}
	}
}

func (f *Fake) authorized(r *http.Request) bool {
	return r.Header.Get("x-acs-dingtalk-access-token") == token
}

func (f *Fake) send(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RobotCode string   `json:"robotCode"`
		UserIDs   []string `json:"userIds"`
		MsgKey    string   `json:"msgKey"`
		MsgParam  string   `json:"msgParam"`
	}
	if !f.authorized(r) || !readJSON(r, &in) || in.RobotCode != ClientID || in.MsgKey != "sampleMarkdown" || len(in.UserIDs) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "InvalidAuthentication"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fail != 0 {
		writeJSON(w, f.Fail, map[string]string{"code": "ServiceUnavailable"})
		return
	}
	if f.ExpireToken > 0 {
		f.ExpireToken--
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "InvalidAuthentication", "message": "token expired"})
		return
	}
	if f.RefuseAfter > 0 && len(f.sent) >= f.RefuseAfter {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalidParameter"})
		return
	}
	if f.Limit > 0 {
		f.Limit--
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"code": "Throttling"})
		return
	}
	who := in.UserIDs[0]
	switch {
	case f.Invalid[who]:
		writeJSON(w, http.StatusOK, map[string]any{"invalidStaffIdList": []string{who}})
		return
	case f.Held[who]:
		writeJSON(w, http.StatusOK, map[string]any{"flowControlledStaffIdList": []string{who}})
		return
	}
	var p struct{ Title, Text string }
	_ = json.Unmarshal([]byte(in.MsgParam), &p)
	f.sent = append(f.sent, Sent{StaffID: who, Title: p.Title, Text: p.Text})
	if f.LoseAnswers > 0 {
		f.LoseAnswers--
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close() // delivered, but the answer never comes back
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"processQueryKey": "synthetic-key"})
}

func (f *Fake) download(w http.ResponseWriter, r *http.Request) {
	var in struct{ DownloadCode, RobotCode string }
	if !f.authorized(r) || !readJSON(r, &in) || in.RobotCode != ClientID {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "InvalidAuthentication"})
		return
	}
	f.mu.Lock()
	scheme := "https"
	if f.PlainDownloads {
		scheme = "http"
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"downloadUrl": scheme + "://" + r.Host + "/files/" + in.DownloadCode})
}

func (f *Fake) file(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	data, ok := f.Files[r.PathValue("code")]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(data)
}

func (f *Fake) webhook(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text struct{ Content string } `json:"text"`
	}
	if !readJSON(r, &in) {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.replies = append(f.replies, in.Text.Content)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"errcode": 0, "errmsg": "ok"})
}

// WaitConnected waits for the next Stream connection.
func (f *Fake) WaitConnected(t testing.TB) {
	t.Helper()
	select {
	case <-f.up:
	case <-time.After(5 * time.Second):
		t.Fatal("no Stream connection")
	}
}

// Push sends a raw frame on the Stream connection.
func (f *Fake) Push(t testing.TB, frame any) {
	t.Helper()
	f.mu.Lock()
	c, ctx := f.conn, f.connCtx
	f.mu.Unlock()
	if c == nil {
		t.Fatal("no Stream connection to push on")
	}
	raw, _ := json.Marshal(frame)
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("pushing a frame: %v", err)
	}
}

// PushMessage sends a robot message (its data as DingTalk sends it) as a callback frame with messageID.
func (f *Fake) PushMessage(t testing.TB, messageID string, data map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(data)
	f.Push(t, map[string]any{"specVersion": "1.0", "type": "CALLBACK", "time": time.Now().UnixMilli(),
		"headers": map[string]string{"topic": "/v1.0/im/bot/messages/get", "messageId": messageID, "contentType": "application/json"},
		"data":    string(raw)})
}

// Acks are the frames the client answered, in order.
func (f *Fake) Acks() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.acks...)
}

// Sent are the messages sent to people, in order; Replies the texts answered through session webhooks.
func (f *Fake) Sent() []Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Sent(nil), f.sent...)
}

func (f *Fake) Replies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.replies...)
}

// Opens is how many Stream connections were asked for.
func (f *Fake) Opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
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

// AckOf is the answer to the frame messageID, or nil.
func (f *Fake) AckOf(messageID string) map[string]any {
	for _, a := range f.Acks() {
		if h, ok := a["headers"].(map[string]any); ok && h["messageId"] == messageID {
			return a
		}
	}
	return nil
}
