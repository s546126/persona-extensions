package tgbot

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
)

func TestHardeningStrangerMessageIDDoesNotDropOwnerMessage(t *testing.T) {
	fg := &fakeTelegram{token: validTestToken}
	srv := httptest.NewServer(fg.handler())
	defer srv.Close()

	hub := &fakeHub{}
	sec := &memSecrets{}
	now := time.Date(2026, 11, 27, 8, 0, 0, 0, time.UTC)
	bot := New(Options{
		Hub:     hub,
		BaseURL: srv.URL,
		Lang:    "en",
		Now:     func() time.Time { return now },
	}, sec)

	c, err := NewClient(srv.URL, validTestToken, "")
	if err != nil {
		t.Fatal(err)
	}
	creds := credentials{
		Token:       validTestToken,
		OwnerID:     777,
		ChatID:      777,
		BotUsername: "persona_test_bot",
		OwnerName:   "Alice",
	}
	ctx := context.Background()

	// In Telegram, message_id is per-chat (each DM starts at 1, 2, 3...).
	// A stranger sending MessageID 1 in chat 888 must neither pollute b.st.Seen
	// nor bump LastEvent, and must NOT cause the owner's MessageID 1 in chat 777 to be dropped.
	bot.handleMessage(ctx, c, creds, Message{
		MessageID: 1,
		Chat:      Chat{ID: 888, Type: "private"},
		From:      &User{ID: 888, FirstName: "Stranger"},
		Text:      "hello from stranger",
	})
	if !bot.Status(ctx).LastEvent.IsZero() {
		t.Fatalf("stranger message bumped LastEvent: %v", bot.Status(ctx).LastEvent)
	}

	bot.handleMessage(ctx, c, creds, Message{
		MessageID: 1,
		Chat:      Chat{ID: 777, Type: "private"},
		From:      &User{ID: 777, FirstName: "Alice"},
		Text:      "owner message with same local id",
	})
	msgs := hub.all()
	if len(msgs) != 1 || msgs[0].Text != "owner message with same local id" {
		t.Fatalf("owner message with colliding per-chat MessageID was dropped: got %+v", msgs)
	}
}

func TestHardeningLogoutCancelsPollAndPreventsStateResurrection(t *testing.T) {
	pollStarted := make(chan struct{}, 1)
	releasePoll := make(chan struct{})
	var canceled atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			select {
			case pollStarted <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
				canceled.Store(true)
				return
			case <-releasePoll:
				resp, _ := json.Marshal(map[string]any{
					"ok": true,
					"result": []Update{{
						UpdateID: 42,
						Message: &Message{
							MessageID: 99,
							Chat:      Chat{ID: 777, Type: "private"},
							From:      &User{ID: 777, FirstName: "Alice"},
							Text:      "late update after logout",
						},
					}},
				})
				_, _ = w.Write(resp)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	hub := &fakeHub{}
	sec := &memSecrets{}
	saved, _ := json.Marshal(state{
		Creds: &credentials{
			Token:       validTestToken,
			OwnerID:     777,
			ChatID:      777,
			BotUsername: "persona_test_bot",
			OwnerName:   "Alice",
		},
	})
	_ = sec.Save(context.Background(), saved)

	bot := New(Options{Hub: hub, BaseURL: srv.URL, Lang: "en"}, sec)
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
		t.Fatal("getUpdates never started")
	}

	if err := bot.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Logout should cancel the in-flight getUpdates promptly rather than waiting 25s.
	deadline := time.Now().Add(2 * time.Second)
	for !canceled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(releasePoll)
	time.Sleep(80 * time.Millisecond)

	if !canceled.Load() {
		t.Fatal("Logout did not cancel in-flight GetUpdates long poll")
	}
	if hub.count() != 0 {
		t.Fatalf("message from logged-out bot reached hub: %+v", hub.all())
	}
	raw, _ := sec.Load(context.Background())
	if len(raw) != 0 {
		t.Fatalf("Logout secret was resurrected by late poll: %s", string(raw))
	}
}

func TestHardeningConcurrentPollPairRace(t *testing.T) {
	var nextID atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":999,"is_bot":true,"first_name":"Bot","username":"test_bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			id := nextID.Add(1)
			resp, _ := json.Marshal(map[string]any{
				"ok": true,
				"result": []Update{{
					UpdateID: id,
					Message: &Message{
						MessageID: id,
						Chat:      Chat{ID: 55, Type: "private"},
						From:      &User{ID: 55},
						Text:      "000000",
					},
				}},
			})
			_, _ = w.Write(resp)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	bot := New(Options{BaseURL: srv.URL, Lang: "en"}, &memSecrets{})
	ctx := context.Background()
	if _, err := bot.LoginStep(ctx, stepToken, map[string]string{"token": validTestToken}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = bot.LoginStep(ctx, stepPair, nil)
		}()
	}
	wg.Wait()
}

func TestHardeningAPIRedirectOversizedProxyScrubAndHeaders(t *testing.T) {
	// 1. Refuse HTTP redirects so the bot token in the URL is never forwarded elsewhere.
	var redirectHits atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectHits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1}}`))
	}))
	defer redirectTarget.Close()

	var multipartCaptured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			http.Redirect(w, r, redirectTarget.URL+"/leak", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			// Valid JSON prefix followed by >maxAPIResponseBytes trailing spaces.
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"f","file_path":"a.txt"}}`))
			_, _ = w.Write([]byte(strings.Repeat(" ", maxAPIResponseBytes)))
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			body, _ := io.ReadAll(r.Body)
			multipartCaptured = string(body)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":123}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, validTestToken, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetMe(context.Background()); err == nil {
		t.Fatal("expected GetMe to reject HTTP redirect")
	}
	if redirectHits.Load() != 0 {
		t.Fatalf("client followed redirect with bot token (%d hits)", redirectHits.Load())
	}

	// 2. Reject oversized JSON responses (> maxAPIResponseBytes) even with a valid JSON prefix.
	if _, err := c.GetFile(context.Background(), "f"); err == nil {
		t.Fatal("expected oversized response to be rejected")
	}

	// 3. Proxy credentials must never leak in error strings.
	proxyClient, err := NewClient("https://127.0.0.1:1", validTestToken, "http://alice:SUPERSECRETPROXYPASS@127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	_, perr := proxyClient.GetMe(context.Background())
	if perr == nil {
		t.Fatal("expected proxy error")
	}
	if strings.Contains(perr.Error(), "SUPERSECRETPROXYPASS") {
		t.Fatalf("proxy password leaked in error: %q", perr.Error())
	}

	// 4. SendPhoto must strip CR/LF from filename and MIME type to prevent header injection.
	if _, err := c.SendPhoto(context.Background(), 1, "evil\"\r\nX-Injected: yes.png", "image/png\r\nX-Evil: 1", []byte("png"), 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(multipartCaptured, "\r\nX-Injected:") || strings.Contains(multipartCaptured, "\r\nX-Evil:") {
		t.Fatalf("CRLF header injection succeeded in multipart body:\n%s", multipartCaptured)
	}

	// 5. DownloadFile must reject query/fragment/backslash injection in filePath.
	for _, badPath := range []string{"a/b?x=1", "a/b#frag", `a\b`, "a\nb"} {
		if _, err := c.DownloadFile(context.Background(), badPath, 1024); err == nil {
			t.Errorf("DownloadFile accepted unsafe path %q", badPath)
		}
	}
}

func TestHardeningSplitTextNonPositiveAndTypingCancel(t *testing.T) {
	if got := splitText("hello", 0); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("splitText with maxRunes=0 returned %v", got)
	}
	if got := splitText("hello", -5); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("splitText with maxRunes=-5 returned %v", got)
	}

	// Typing(true) with a slow sendChatAction must not block Logout / Typing(false).
	actionStarted := make(chan struct{}, 1)
	actionCanceled := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/sendChatAction") {
			select {
			case actionStarted <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
				select {
				case actionCanceled <- struct{}{}:
				default:
				}
			case <-time.After(5 * time.Second):
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	bot := New(Options{BaseURL: srv.URL, Lang: "en"}, &memSecrets{})
	bot.mu.Lock()
	bot.st.Creds = &credentials{Token: validTestToken, OwnerID: 777, ChatID: 777}
	bot.loaded = true
	bot.mu.Unlock()

	typingDone := make(chan error, 1)
	go func() {
		typingDone <- bot.Typing(context.Background(), true)
	}()

	select {
	case <-actionStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("sendChatAction did not start")
	}

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		_ = bot.Typing(context.Background(), false)
	}()

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Typing(false) deadlocked waiting on slow initial sendChatAction")
	}
	select {
	case <-actionCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("Typing(false) did not cancel in-flight sendChatAction")
	}
	<-typingDone
}
