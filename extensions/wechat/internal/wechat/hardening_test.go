package wechat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wechat/internal/wechat/ilink"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHardeningConcurrentLoginStepPendingQRRace(t *testing.T) {
	var toggle atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			_, _ = w.Write([]byte(`{"ret":0,"qrcode":"qr-1","qrcode_img_content":"https://ilinkai.weixin.qq.com/qr/1"}`))
		case "/ilink/bot/get_qrcode_status":
			n := toggle.Add(1)
			if n%2 == 0 {
				_, _ = w.Write([]byte(`{"ret":0,"status":"scaned"}`))
			} else {
				_, _ = w.Write([]byte(`{"ret":0,"status":"scaned_but_redirect","redirect_host":"ilinkai.weixin.qq.com"}`))
			}
		default:
			_, _ = w.Write([]byte(`{"ret":0}`))
		}
	}))
	defer srv.Close()

	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		r2 := req.Clone(req.Context())
		r2.URL.Scheme = "http"
		r2.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		r2.Host = r2.URL.Host
		return http.DefaultTransport.RoundTrip(r2)
	})
	bot := New(Options{
		Lang: "en",
		NewClient: func(base, token string) (*ilink.Client, error) {
			return ilink.New(ilink.Options{BaseURL: base, Token: token, HTTP: &http.Client{Transport: rt}})
		},
	}, &memSecrets{})

	ctx := context.Background()
	if _, err := bot.LoginStep(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				_, _ = bot.LoginStep(ctx, stepQR, nil)
			}
		}()
	}
	wg.Wait()
}

func TestHardeningConcurrentTypingRace(t *testing.T) {
	f := newFakeILink(t)
	now := time.Date(2026, 11, 27, 8, 0, 0, 0, time.UTC)
	var nowMu sync.Mutex
	bot := New(Options{
		Lang: "en",
		Now: func() time.Time {
			nowMu.Lock()
			defer nowMu.Unlock()
			now = now.Add(2 * time.Second)
			return now
		},
		NewClient: func(base, token string) (*ilink.Client, error) {
			return ilink.New(ilink.Options{BaseURL: base, Token: token, HTTP: &http.Client{Transport: f}})
		},
	}, &memSecrets{})
	c, err := bot.opt.NewClient(ilink.AuthOrigin, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	bot.mu.Lock()
	bot.loaded = true
	bot.client = c
	bot.st.Creds = &ilink.Credentials{Token: "tok-1", BotID: "b1", UserID: "u1", BaseURL: ilink.AuthOrigin}
	bot.st.Context = "ctx-1"
	bot.mu.Unlock()

	ctx := context.Background()
	if err := bot.Typing(ctx, true); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				_ = bot.Typing(ctx, true)
			}
		}()
	}
	wg.Wait()
}

func TestHardeningLogoutCancelsPollAndPreventsStaleDeliveryOrContextCorruption(t *testing.T) {
	pollStarted := make(chan struct{}, 1)
	releasePoll := make(chan struct{})
	var pollCanceled atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/ilink/bot/getupdates":
			select {
			case pollStarted <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
				pollCanceled.Store(true)
				return
			case <-releasePoll:
				_, _ = w.Write([]byte(`{"ret":0,"get_updates_buf":"cur-old","msgs":[{"client_id":"m-old","from_user_id":"owner-a","context_token":"ctx-from-a","message_type":1,"item_list":[{"type":1,"text_item":{"text":"stale message"}}]}]}`))
				return
			}
		default:
			_, _ = w.Write([]byte(`{"ret":0}`))
		}
	}))
	defer srv.Close()

	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		r2 := req.Clone(req.Context())
		r2.URL.Scheme = "http"
		r2.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		r2.Host = r2.URL.Host
		return http.DefaultTransport.RoundTrip(r2)
	})
	hub := &testHub{}
	sec := &memSecrets{}
	saved, _ := json.Marshal(state{
		Creds: &ilink.Credentials{Token: "tok-a", BotID: "bot-a", UserID: "owner-a", BaseURL: ilink.AuthOrigin},
	})
	_ = sec.Save(context.Background(), saved)

	bot := New(Options{
		Hub:  hub,
		Lang: "en",
		NewClient: func(base, token string) (*ilink.Client, error) {
			return ilink.New(ilink.Options{BaseURL: base, Token: token, HTTP: &http.Client{Transport: rt}})
		},
	}, sec)

	runCtx, stopRun := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		bot.Run(runCtx)
	}()
	defer func() {
		stopRun()
		<-runDone
	}()

	select {
	case <-pollStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("GetUpdates never started")
	}

	if err := bot.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !pollCanceled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(releasePoll)
	time.Sleep(80 * time.Millisecond)

	if !pollCanceled.Load() {
		t.Fatal("Logout did not cancel in-flight GetUpdates long poll")
	}
	if len(hub.all()) != 0 {
		t.Fatalf("stale message delivered after Logout: %+v", hub.all())
	}
	st := sec.state(t)
	if st.Context != "" || st.Creds != nil {
		t.Fatalf("stale poll corrupted post-Logout state: %+v", st)
	}
}

func TestHardeningStrangerLastEventAndFileNameSanitized(t *testing.T) {
	f := newFakeILink(t)
	keyHex := "00112233445566778899aabbccddeeff"
	keyBytes := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	enc, err := ilink.EncryptECB([]byte("file body"), keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.downloads = map[string][]byte{"q-doc": enc}
	f.mu.Unlock()

	hub := &testHub{}
	sec := &memSecrets{}
	bot := New(Options{
		Hub:  hub,
		Lang: "en",
		NewClient: func(base, token string) (*ilink.Client, error) {
			return ilink.New(ilink.Options{BaseURL: base, Token: token, HTTP: &http.Client{Transport: f}})
		},
	}, sec)
	c, err := bot.opt.NewClient(ilink.AuthOrigin, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	creds := ilink.Credentials{Token: "tok-1", BotID: "b1", UserID: "owner-1", BaseURL: ilink.AuthOrigin}
	bot.mu.Lock()
	bot.loaded = true
	bot.client = c
	bot.st.Creds = &creds
	bot.mu.Unlock()

	ctx := context.Background()
	// Stranger message must not bump LastEvent or pollute owner's Seen list.
	bot.handle(ctx, c, creds, ilink.Message{ID: "msg-1", From: "stranger-1", ContextToken: "ctx-s", Text: "hi"})
	if !bot.Status(ctx).LastEvent.IsZero() {
		t.Fatalf("stranger message bumped LastEvent: %v", bot.Status(ctx).LastEvent)
	}

	// Owner file with traversal and CRLF characters must be sanitized.
	bot.handle(ctx, c, creds, ilink.Message{
		ID:           "msg-2",
		From:         "owner-1",
		ContextToken: "ctx-o",
		Media: []ilink.Media{{
			Kind:     ilink.MediaFile,
			Query:    "q-doc",
			KeyHex:   keyHex,
			FileName: "../../secret\r\nreport.pdf",
		}},
	})
	msgs := hub.all()
	if len(msgs) != 1 || len(msgs[0].Files) != 1 {
		t.Fatalf("expected 1 file message, got %+v", msgs)
	}
	if msgs[0].Files[0].Name != "secretreport.pdf" {
		t.Fatalf("unsafe filename was not sanitized: %q", msgs[0].Files[0].Name)
	}
}

type testHub struct {
	mu   sync.Mutex
	msgs []wire.ChannelMessage
}

func (h *testHub) Receive(_ context.Context, m wire.ChannelMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, m)
	return nil
}

func (h *testHub) all() []wire.ChannelMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]wire.ChannelMessage(nil), h.msgs...)
}
