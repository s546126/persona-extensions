package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/github/internal/connector"
)

func TestPollDeviceConcurrentSlowDownNoRace(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dc1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
		case "/login/oauth/access_token":
			n := calls.Add(1)
			if n%2 == 1 {
				_, _ = w.Write([]byte(`{"error":"slow_down","interval":10}`))
			} else {
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := New(Config{}, "", nil)
	c.web, c.api = srv.URL, srv.URL
	c.UseSecrets(&memSecrets{})
	if _, err := c.LoginStep(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				_, _ = c.LoginStep(context.Background(), "device", nil)
			}
		}()
	}
	wg.Wait()
}

func TestLogoutDuringInFlightPollDeviceDoesNotResurrectToken(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dc1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
		case "/login/oauth/access_token":
			_, _ = io.Copy(io.Discard, r.Body)
			close(entered)
			<-release
			_, _ = w.Write([]byte(`{"access_token":"gho_resurrected_token","token_type":"bearer"}`))
		case "/user":
			_, _ = w.Write([]byte(`{"login":"alice"}`))
		}
	}))
	t.Cleanup(srv.Close)

	sec := &memSecrets{}
	c := New(Config{}, "", nil)
	c.web, c.api = srv.URL, srv.URL
	c.UseSecrets(sec)
	if _, err := c.LoginStep(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}

	done := make(chan connector.Prompt, 1)
	go func() {
		p, _ := c.LoginStep(context.Background(), "device", nil)
		done <- p
	}()

	<-entered
	if err := c.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	p := <-done
	if p.Done {
		t.Fatalf("stale pollDevice completed login after Logout: %+v", p)
	}
	raw, _ := sec.Load(context.Background())
	var kept deviceLogin
	_ = json.Unmarshal(raw, &kept)
	if kept.Token != "" {
		t.Fatalf("Logout token was resurrected into secrets: %q", kept.Token)
	}
	if st := c.Status(context.Background()); st.State != connector.StateNotSignedIn {
		t.Fatalf("expected signed out state, got %+v", st)
	}
}

func TestLogoutAndSourceSwitchClearAccountCachesAndAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			if r.Header.Get("Authorization") != "Bearer gho_alice" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"login":"alice"}`))
		case "/graphql":
			_, _ = w.Write([]byte(`{"data":{"search":{"nodes":[{"number":7,"title":"Secret PR","url":"https://github.com/org/private/pull/7","updatedAt":"2026-11-27T10:00:00Z","repository":{"nameWithOwner":"org/private"},"author":{"login":"alice"}}]}}}`))
		case "/repos/org/private/issues/7":
			w.Header().Set("ETag", `"etag-alice"`)
			_, _ = w.Write([]byte(`{"title":"Secret Issue","state":"open","user":{"login":"alice","avatar_url":"https://avatars.example/alice?v=1"}}`))
		case "/repos/org/private/actions/jobs/99/logs":
			_, _ = w.Write([]byte("secret-build-log"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	sec := &memSecrets{data: []byte(`{"token":"gho_alice"}`)}
	c := New(Config{}, "", nil)
	c.api, c.web = srv.URL, srv.URL
	c.UseSecrets(sec)
	c.check(ctx)

	if st := c.Status(ctx); st.State != connector.StateOK || st.Account != "alice" {
		t.Fatalf("initial status: %+v", st)
	}
	// Populate list cache, etag cache, job log cache, avatar cache, and inbox state.
	if entries, _, err := c.List(ctx, TabReview, true); err != nil || len(entries) != 1 {
		t.Fatalf("List: %v %v", entries, err)
	}
	var iss threadIssue
	if err := c.cachedGet(ctx, "/repos/org/private/issues/7", &iss); err != nil {
		t.Fatalf("cachedGet: %v", err)
	}
	c.threadPerson(iss.User)
	if log, ok := c.jobLog(ctx, "org/private", 99); !ok || log == "" {
		t.Fatalf("jobLog: %q %v", log, ok)
	}
	c.inbox.mu.Lock()
	c.inbox.things = map[string]*connector.InboxThing{"org/private#7": {Thread: "org/private#7", Title: "Secret"}}
	c.inbox.seen = map[string]string{"n1": "2026-11-27T10:00:00Z"}
	c.inbox.lastModified = "Wed, 27 Nov 2026 10:00:00 GMT"
	c.inbox.mu.Unlock()

	if err := c.Logout(ctx); err != nil {
		t.Fatal(err)
	}

	if st := c.Status(ctx); st.State != connector.StateNotSignedIn || st.Account != "" {
		t.Fatalf("Status after Logout leaked previous Account: %+v", st)
	}
	c.lists.mu.Lock()
	listLen := len(c.lists.tabs)
	c.lists.mu.Unlock()
	if listLen != 0 {
		t.Fatalf("lists cache retained %d tab(s) after Logout", listLen)
	}
	c.etags.mu.Lock()
	etagLen, logLen, avatarLen := len(c.etags.m), len(c.etags.logs), len(c.etags.avatars)
	c.etags.mu.Unlock()
	if etagLen != 0 || logLen != 0 || avatarLen != 0 {
		t.Fatalf("etagCache retained entries after Logout: m=%d logs=%d avatars=%d", etagLen, logLen, avatarLen)
	}
	c.inbox.mu.Lock()
	inboxLen, seenLen, lm := len(c.inbox.things), len(c.inbox.seen), c.inbox.lastModified
	c.inbox.mu.Unlock()
	if inboxLen != 0 || seenLen != 0 || lm != "" {
		t.Fatalf("inbox state retained data after Logout: things=%d seen=%d lm=%q", inboxLen, seenLen, lm)
	}

	// Also verify UseMachine clears caches populated by a prior machine login.
	c.UseMachine(ctx, "m1", "Mac 1", func(context.Context, string) (string, error) { return "gho_alice", nil })
	c.check(ctx)
	if entries, _, err := c.List(ctx, TabReview, true); err != nil || len(entries) != 1 {
		t.Fatalf("machine List: %v %v", entries, err)
	}
	c.UseMachine(ctx, "m2", "Mac 2", func(context.Context, string) (string, error) { return "gho_bob", nil })
	c.lists.mu.Lock()
	afterSwitch := len(c.lists.tabs)
	c.lists.mu.Unlock()
	if afterSwitch != 0 {
		t.Fatalf("UseMachine retained %d cached list tab(s) from prior machine", afterSwitch)
	}
}

func TestRequestPropagatesBodyReadErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hijack and abruptly close TCP connection to induce io.ErrUnexpectedEOF on client read.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := New(Config{}, "dev", func(context.Context, string) (string, error) { return "tok", nil })
	c.api = srv.URL
	c.now = func() time.Time { return time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC) }

	if raw, err := c.raw(context.Background(), "/repos/o/r/actions/jobs/1/logs"); err == nil {
		t.Fatalf("raw swallowed mid-stream EOF and returned %q with nil error", raw)
	}
}
