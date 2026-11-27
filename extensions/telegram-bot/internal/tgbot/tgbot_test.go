package tgbot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

type memSecrets struct {
	mu   sync.Mutex
	data []byte
}

func (m *memSecrets) Load(context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.data...), nil
}

func (m *memSecrets) Save(_ context.Context, d []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = append([]byte(nil), d...)
	return nil
}

func (m *memSecrets) Delete(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = nil
	return nil
}

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

func (h *fakeHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

func (h *fakeHub) all() []wire.ChannelMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]wire.ChannelMessage(nil), h.msgs...)
}

type fakeTelegram struct {
	mu          sync.Mutex
	updates     []Update
	sentText    []map[string]any
	sentPhotos  int
	typingCount int
	files       map[string][]byte
	token       string
	reject401   bool
}

func (f *fakeTelegram) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		tok := f.token
		reject := f.reject401
		f.mu.Unlock()

		prefix := "/bot" + tok + "/"
		filePrefix := "/file/bot" + tok + "/"
		if strings.HasPrefix(r.URL.Path, filePrefix) {
			rel := strings.TrimPrefix(r.URL.Path, filePrefix)
			f.mu.Lock()
			data, ok := f.files[rel]
			f.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
			return
		}
		if !strings.HasPrefix(r.URL.Path, prefix) || reject {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		method := strings.TrimPrefix(r.URL.Path, prefix)
		switch method {
		case "getMe":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":999,"is_bot":true,"first_name":"PersonaBot","username":"persona_test_bot"}}`))
		case "getUpdates":
			var req struct {
				Offset int64 `json:"offset"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			var out []Update
			for _, u := range f.updates {
				if u.UpdateID >= req.Offset {
					out = append(out, u)
				}
			}
			f.mu.Unlock()
			resp, _ := json.Marshal(map[string]any{"ok": true, "result": out})
			_, _ = w.Write(resp)
		case "sendMessage":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.sentText = append(f.sentText, body)
			id := 500 + len(f.sentText)
			f.mu.Unlock()
			resp, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
			_, _ = w.Write(resp)
		case "sendPhoto":
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fh, _, err := r.FormFile("photo")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = io.ReadAll(fh)
			_ = fh.Close()
			f.mu.Lock()
			f.sentPhotos++
			id := 800 + f.sentPhotos
			f.mu.Unlock()
			resp, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
			_, _ = w.Write(resp)
		case "sendChatAction":
			f.mu.Lock()
			f.typingCount++
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case "getFile":
			var req struct {
				FileID string `json:"file_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			data, ok := f.files[req.FileID]
			f.mu.Unlock()
			if !ok {
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"file not found"}`))
				return
			}
			resp, _ := json.Marshal(map[string]any{
				"ok":     true,
				"result": map[string]any{"file_id": req.FileID, "file_size": len(data), "file_path": req.FileID},
			})
			_, _ = w.Write(resp)
		default:
			http.NotFound(w, r)
		}
	})
}

const validTestToken = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func TestLoginValidationAndRedaction(t *testing.T) {
	fg := &fakeTelegram{token: validTestToken}
	srv := httptest.NewServer(fg.handler())
	defer srv.Close()

	bot := New(Options{BaseURL: srv.URL, Lang: "en"}, &memSecrets{})
	ctx := context.Background()

	p, err := bot.LoginStep(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Step != stepToken || len(p.Fields) != 2 {
		t.Fatalf("unexpected initial prompt: %+v", p)
	}

	p, err = bot.LoginStep(ctx, stepToken, map[string]string{"token": "bad"})
	if err != nil || p.Error == "" {
		t.Fatalf("expected token validation error, got prompt=%+v err=%v", p, err)
	}

	p, err = bot.LoginStep(ctx, stepToken, map[string]string{"token": validTestToken, "proxy": "socks5://127.0.0.1:1080"})
	if err != nil || p.Error == "" {
		t.Fatalf("expected proxy validation error, got prompt=%+v err=%v", p, err)
	}

	// Wrong token should scrub token in any returned error string.
	badTok := "999999999:SECRETTOKENSHOULDNOTLEAK123"
	p, err = bot.LoginStep(ctx, stepToken, map[string]string{"token": badTok})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.Error, badTok) {
		t.Fatalf("token leaked in error: %q", p.Error)
	}
}

func TestTwoStepPairingAndOwnerOnlyDelivery(t *testing.T) {
	fg := &fakeTelegram{
		token: validTestToken,
		files: map[string][]byte{
			"photos/pic.jpg": {0xff, 0xd8, 0xff, 0xd9},
			"docs/note.txt":  []byte("hello doc"),
		},
	}
	srv := httptest.NewServer(fg.handler())
	defer srv.Close()

	hub := &fakeHub{}
	sec := &memSecrets{}
	now := time.Date(2026, 11, 27, 6, 0, 0, 0, time.UTC)
	bot := New(Options{
		Hub:     hub,
		BaseURL: srv.URL,
		Lang:    "en",
		Now:     func() time.Time { return now },
	}, sec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Step 1: submit valid token.
	p, err := bot.LoginStep(ctx, stepToken, map[string]string{"token": validTestToken})
	if err != nil || p.Step != stepPair || len(p.UserCode) != 6 {
		t.Fatalf("unexpected pair prompt: %+v err=%v", p, err)
	}
	code := p.UserCode

	// Wrong code in group is ignored; wrong code in private increments attempts.
	fg.mu.Lock()
	fg.updates = []Update{
		{UpdateID: 1, Message: &Message{MessageID: 10, Chat: Chat{ID: -100, Type: "group"}, From: &User{ID: 42}, Text: code}},
		{UpdateID: 2, Message: &Message{MessageID: 11, Chat: Chat{ID: 777, Type: "private"}, From: &User{ID: 777, FirstName: "Alice"}, Text: "000000"}},
		{UpdateID: 3, Message: &Message{MessageID: 12, Chat: Chat{ID: 777, Type: "private"}, From: &User{ID: 777, FirstName: "Alice"}, Text: code}},
	}
	fg.mu.Unlock()

	p, err = bot.LoginStep(ctx, stepPair, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Done || !strings.Contains(p.Account, "Alice") {
		t.Fatalf("expected pairing done for Alice, got %+v", p)
	}

	// Verify incoming messages: group dropped, stranger rejected once, owner photo+doc+quote forwarded.
	c, err := NewClient(srv.URL, validTestToken, "")
	if err != nil {
		t.Fatal(err)
	}
	creds := credentials{Token: validTestToken, OwnerID: 777, ChatID: 777, BotUsername: "persona_test_bot", OwnerName: "Alice"}

	bot.handleMessage(ctx, c, creds, Message{MessageID: 20, Chat: Chat{ID: -200, Type: "supergroup"}, From: &User{ID: 777}, Text: "hi"})
	bot.handleMessage(ctx, c, creds, Message{MessageID: 21, Chat: Chat{ID: 888, Type: "private"}, From: &User{ID: 888}, Text: "stranger 1"})
	bot.handleMessage(ctx, c, creds, Message{MessageID: 22, Chat: Chat{ID: 888, Type: "private"}, From: &User{ID: 888}, Text: "stranger 2"})
	if hub.count() != 0 {
		t.Fatalf("expected 0 hub messages, got %d", hub.count())
	}

	// Only 1 stranger reply within 10 minutes + 2 pairing replies (wrong + paired).
	fg.mu.Lock()
	sentBefore := len(fg.sentText)
	fg.mu.Unlock()
	if sentBefore != 3 {
		t.Fatalf("expected 3 bot messages (2 pairing + 1 stranger), got %d", sentBefore)
	}

	bot.handleMessage(ctx, c, creds, Message{
		MessageID:      23,
		Chat:           Chat{ID: 777, Type: "private"},
		From:           &User{ID: 777, FirstName: "Alice"},
		Caption:        "look at this",
		ReplyToMessage: &Message{MessageID: 5, Text: "original question"},
		Photo:          []PhotoSize{{FileID: "photos/pic.jpg", Width: 800, Height: 600, FileSize: 4}},
		Document:       &Document{FileID: "docs/note.txt", FileName: "note.txt", MimeType: "text/plain", FileSize: 9},
	})

	// Duplicate delivery of MessageID 23 must be ignored.
	bot.handleMessage(ctx, c, creds, Message{
		MessageID: 23,
		Chat:      Chat{ID: 777, Type: "private"},
		From:      &User{ID: 777, FirstName: "Alice"},
		Text:      "dup",
	})

	msgs := hub.all()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 hub message, got %d", len(msgs))
	}
	got := msgs[0]
	if got.Text != "look at this" || got.Quote != "original question" || got.ReplyTo != "5" {
		t.Fatalf("unexpected inbound message: %+v", got)
	}
	if len(got.Images) != 1 || len(got.Files) != 1 || got.Owner == nil || !*got.Owner {
		t.Fatalf("unexpected media/owner fields: %+v", got)
	}

	// Outbound Send with chunking and photo + Typing + Logout.
	longText := strings.Repeat("a", MaxText+50)
	imgB64 := base64.StdEncoding.EncodeToString([]byte("pngdata"))
	res, err := bot.Send(ctx, wire.ChannelSendParams{
		Contribution: ID,
		Ref:          "ref-1",
		Text:         longText,
		ReplyTo:      "23",
		Images:       []wire.ChannelBlob{{Name: "out.png", Mime: "image/png", Data: imgB64}},
	})
	if err != nil || res.MessageID == "" {
		t.Fatalf("send failed: res=%+v err=%v", res, err)
	}
	fg.mu.Lock()
	photos := fg.sentPhotos
	texts := len(fg.sentText) - sentBefore
	fg.mu.Unlock()
	if texts != 2 || photos != 1 {
		t.Fatalf("expected 2 text chunks + 1 photo, got texts=%d photos=%d", texts, photos)
	}

	if err := bot.Typing(ctx, true); err != nil {
		t.Fatal(err)
	}
	_ = bot.Typing(ctx, false)
	fg.mu.Lock()
	tc := fg.typingCount
	fg.mu.Unlock()
	if tc == 0 {
		t.Fatal("expected sendChatAction typing call")
	}

	if err := bot.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if bot.Status(ctx).State != wire.ConnectorStateNotSignedIn {
		t.Fatalf("expected not_signed_in after logout, got %+v", bot.Status(ctx))
	}
}

func TestPairingLockoutAndExpiry(t *testing.T) {
	fg := &fakeTelegram{token: validTestToken}
	srv := httptest.NewServer(fg.handler())
	defer srv.Close()

	now := time.Date(2026, 11, 27, 6, 0, 0, 0, time.UTC)
	bot := New(Options{BaseURL: srv.URL, Lang: "en", Now: func() time.Time { return now }}, &memSecrets{})
	ctx := context.Background()

	_, err := bot.LoginStep(ctx, stepToken, map[string]string{"token": validTestToken})
	if err != nil {
		t.Fatal(err)
	}
	fg.mu.Lock()
	for i := 1; i <= pairMaxWrong; i++ {
		fg.updates = append(fg.updates, Update{
			UpdateID: int64(i),
			Message:  &Message{MessageID: int64(10 + i), Chat: Chat{ID: 55, Type: "private"}, From: &User{ID: 55}, Text: "999999"},
		})
	}
	fg.mu.Unlock()

	p, err := bot.LoginStep(ctx, stepPair, nil)
	if err != nil || p.Step != stepToken || p.Error == "" {
		t.Fatalf("expected lockout back to token prompt, got %+v err=%v", p, err)
	}

	// Expiry check.
	_, err = bot.LoginStep(ctx, stepToken, map[string]string{"token": validTestToken})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(pairMaxAge + time.Second)
	p, err = bot.LoginStep(ctx, stepPair, nil)
	if err != nil || p.Step != stepToken || p.Error == "" {
		t.Fatalf("expected expiry back to token prompt, got %+v err=%v", p, err)
	}
}
