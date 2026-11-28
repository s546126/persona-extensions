package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/dingtalk/internal/dingtalk/api"
	"github.com/nwverse/persona/sdk/go/wire"
)

type blockingHub struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	msgs    []wire.ChannelMessage
}

func (h *blockingHub) Receive(_ context.Context, m wire.ChannelMessage) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, m)
	h.mu.Unlock()
	if h.entered != nil {
		select {
		case h.entered <- struct{}{}:
		default:
		}
	}
	if h.release != nil {
		<-h.release
	}
	return nil
}

type memStore struct {
	mu      sync.Mutex
	data    []byte
	saves   int
	deletes int
}

func (s *memStore) Load(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...), nil
}

func (s *memStore) Save(_ context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.data = append([]byte(nil), data...)
	return nil
}

func (s *memStore) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	s.data = nil
	return nil
}

func TestLogoutPreventsPostLogoutSecretResurrection(t *testing.T) {
	store := &memStore{}
	initial, _ := json.Marshal(state{
		ClientID:     "app-1",
		ClientSecret: "sec-1",
		Owner:        &owner{StaffID: "staff-1", Name: "Alice", PairedAt: time.Now()},
	})
	store.data = initial

	hub := &blockingHub{entered: make(chan struct{}, 1), release: make(chan struct{})}
	b := New(Options{Hub: hub}, store)
	if err := b.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	gen := b.sessionGen
	b.mu.Unlock()

	msg := api.Message{
		ID:               "msg-1",
		ConversationType: api.ConversationSingle,
		SenderStaffID:    "staff-1",
		SenderNick:       "Alice",
		Kind:             api.KindText,
	}
	msg.Text.Content = "hello"

	done := make(chan bool, 1)
	go func() {
		done <- b.handle(context.Background(), nil, gen, msg)
	}()

	<-hub.entered
	if err := b.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	close(hub.release)
	<-done

	store.mu.Lock()
	leaked := len(store.data) > 0
	store.mu.Unlock()
	if leaked {
		t.Fatalf("post-logout secret was resurrected: %s", string(store.data))
	}
	if st := b.Status(context.Background()); st.State != wire.ConnectorStateNotSignedIn || !st.LastEvent.IsZero() {
		t.Fatalf("unexpected post-logout status: %+v", st)
	}
}

func TestLogoutDropsStaleQueuedMessages(t *testing.T) {
	store := &memStore{}
	hub := &blockingHub{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			_, _ = w.Write([]byte(`{"accessToken":"tok","expireIn":7200}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	b := New(Options{
		Hub:  hub,
		Code: func() string { return "123456" },
		NewClient: func(id, secret string) (*api.Client, error) {
			return api.New(api.Options{ClientID: id, ClientSecret: secret, BaseURL: srv.URL, HTTP: srv.Client()})
		},
	}, store)

	// Bind Bot A and record its generation.
	if _, err := b.LoginStep(context.Background(), stepCreds, map[string]string{"client_id": "app-a", "client_secret": "sec-a"}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	genA := b.sessionGen
	b.mu.Unlock()

	// Re-bind to Bot B (new generation, new pairing code).
	if _, err := b.LoginStep(context.Background(), stepCreds, map[string]string{"client_id": "app-b", "client_secret": "sec-b"}); err != nil {
		t.Fatal(err)
	}

	// A stale message from Bot A carrying Bot B's pairing code must be discarded, NOT pair Bot B!
	stale := api.Message{
		ID:               "stale-1",
		ConversationType: api.ConversationSingle,
		SenderStaffID:    "attacker",
		Kind:             api.KindText,
	}
	stale.Text.Content = "123456"
	b.handle(context.Background(), nil, genA, stale)
	if st := b.Status(context.Background()); st.Account != "" {
		t.Fatalf("stale message from old session paired new bot: %+v", st)
	}
}

func TestSentRefsScopedByGenerationAndRespectsContext(t *testing.T) {
	var sr sentRefs
	started := make(chan struct{})
	release := make(chan struct{})

	go func() {
		_, _ = sr.do(context.Background(), "1:ref-1", func() (wire.ChannelSendResult, error) {
			close(started)
			<-release
			return wire.ChannelSendResult{}, nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sr.do(ctx, "1:ref-1", func() (wire.ChannelSendResult, error) {
		t.Fatal("duplicate ref should wait, not execute")
		return wire.ChannelSendResult{}, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter got %v, want context.Canceled", err)
	}
	close(release)

	// Different session generation with the same ref must send again.
	var calls atomic.Int32
	if _, err := sr.do(context.Background(), "2:ref-1", func() (wire.ChannelSendResult, error) {
		calls.Add(1)
		return wire.ChannelSendResult{}, nil
	}); err != nil {
		t.Fatalf("second generation send failed: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 send for new generation, got %d", calls.Load())
	}
}

func TestChunkNonPositiveMostDoesNotLoop(t *testing.T) {
	for _, most := range []int{0, -5} {
		got := Chunk("hello", most)
		if len(got) != 1 || got[0] != "hello" {
			t.Fatalf("Chunk(\"hello\", %d) = %v, want [hello]", most, got)
		}
	}
}

func TestComposeSanitizesInboundFileName(t *testing.T) {
	var dlURL string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			_, _ = w.Write([]byte(`{"accessToken":"tok","expireIn":7200}`))
		case "/v1.0/robot/messageFiles/download":
			_, _ = w.Write([]byte(`{"downloadUrl":"` + dlURL + `"}`))
		case "/blob":
			_, _ = w.Write([]byte("payload"))
		}
	}))
	defer srv.Close()
	dlURL = srv.URL + "/blob"

	c, err := api.New(api.Options{ClientID: "id", ClientSecret: "sec", BaseURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	b := New(Options{}, &memStore{})
	content, _ := json.Marshal(map[string]string{
		"downloadCode": "dc1",
		"fileName":     "../../dir\\evil\r\nname.txt",
	})
	m := api.Message{ID: "m1", Kind: api.KindFile, Content: content}
	got := b.compose(context.Background(), c, m)
	if len(got.Files) != 1 {
		t.Fatalf("expected 1 file, got %+v", got)
	}
	if strings.ContainsAny(got.Files[0].Name, `/\`+"\r\n") || strings.Contains(got.Files[0].Name, "..") {
		t.Fatalf("unsanitized filename: %q", got.Files[0].Name)
	}
	if got.Files[0].Name != "evilname.txt" {
		t.Fatalf("got filename %q, want evilname.txt", got.Files[0].Name)
	}
}
