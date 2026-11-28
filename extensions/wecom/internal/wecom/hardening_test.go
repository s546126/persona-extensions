package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wecom/internal/wecom/api"
	"github.com/nwverse/persona-extensions/extensions/wecom/internal/wecom/api/apitest"
	"github.com/nwverse/persona/sdk/go/wire"
)

type blockingHub struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	msgs    []wire.ChannelMessage
}

func (h *blockingHub) Receive(_ context.Context, m wire.ChannelMessage) error {
	select {
	case h.started <- struct{}{}:
	default:
	}
	<-h.release
	h.mu.Lock()
	h.msgs = append(h.msgs, m)
	h.mu.Unlock()
	return nil
}

func TestHardeningLogoutPreventsInFlightSaveResurrectionAndQueuedDrain(t *testing.T) {
	f := apitest.New(t)
	hub := &blockingHub{started: make(chan struct{}, 1), release: make(chan struct{})}
	sec := &memSecrets{}
	bot := New(Options{
		Hub:        hub,
		KickedWait: time.Hour,
		NewClient:  func(id, secret string) (*api.Client, error) { return api.New(f.Options(id, secret)) },
		Code:       func() string { return code },
	}, sec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bot.Run(ctx)

	if _, err := bot.LoginStep(ctx, stepCreds, map[string]string{"bot_id": apitest.BotID, "secret": apitest.Secret}); err != nil {
		t.Fatal(err)
	}
	f.WaitConnected(t)
	f.WaitConnected(t)
	apitest.Eventually(t, "session", func() bool {
		bot.mu.Lock()
		defer bot.mu.Unlock()
		return bot.session != nil
	})
	f.Text(t, "m-pair", ownerID, code)
	apitest.Eventually(t, "paired", func() bool { return bot.Status(ctx).State == wire.ConnectorStateOk })

	// Send two messages: m1 blocks inside Hub.Receive while m2 stays queued in b.queue.
	f.Text(t, "m-inflight", ownerID, "first")
	f.Text(t, "m-queued", ownerID, "second")

	select {
	case <-hub.started:
	case <-time.After(3 * time.Second):
		t.Fatal("Hub.Receive did not start")
	}

	// Logout while m-inflight is in Hub.Receive and m-queued is sitting in b.queue.
	if err := bot.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	close(hub.release)
	time.Sleep(150 * time.Millisecond)

	raw, _ := sec.Load(ctx)
	if len(raw) != 0 {
		t.Fatalf("Logout secret was resurrected by in-flight or queued message: %s", string(raw))
	}
	if !bot.Status(ctx).LastEvent.IsZero() {
		t.Fatalf("LastEvent was not cleared or was resurrected after Logout: %v", bot.Status(ctx).LastEvent)
	}
}

func TestHardeningSentRefsContextCancelAndRebindGeneration(t *testing.T) {
	// 1. Waiting on an in-flight ref via program.channel must respect ctx cancellation.
	r := newRig(t)
	r.pair()
	p := &program{bot: r.bot}
	release := make(chan struct{})
	started := make(chan struct{})
	r.f.Set(func(x *apitest.Fake) { x.LoseAnswers = 1 }) // causes send to wait AnswerWait (10s)
	raw1, _ := json.Marshal(wire.ChannelSendParams{Contribution: ID, Ref: "ref-hang", Text: "hello 1", Kind: wire.ChannelSendKindAnswer})
	go func() {
		close(started)
		_, _ = p.channel(context.Background(), "send", raw1)
		close(release)
	}()
	<-started
	time.Sleep(50 * time.Millisecond)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer waitCancel()
	done := make(chan error, 1)
	go func() {
		_, err := p.channel(waitCtx, "send", raw1)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error when waiter ctx canceled")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("channel.send ignored waiter ctx cancellation while waiting on earlier send")
	}

	// 2. Re-using the same ref after Logout + re-pair must send to the new binding rather than replaying old result.
	r2 := newRig(t)
	r2.pair()
	p2 := &program{bot: r2.bot}
	raw2, _ := json.Marshal(wire.ChannelSendParams{Contribution: ID, Ref: "same-ref", Text: "hello 2", Kind: wire.ChannelSendKindAnswer})
	if _, err := p2.channel(context.Background(), "send", raw2); err != nil {
		t.Fatal(err)
	}
	if err := r2.bot.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := r2.login(apitest.Secret); p.Kind != wire.ConnectorPromptKindDeviceCode || p.UserCode != code {
		t.Fatalf("after re-login: %+v", p)
	}
	r2.f.WaitConnected(t)
	r2.f.WaitConnected(t)
	apitest.Eventually(t, "re-bound session", func() bool {
		r2.bot.mu.Lock()
		defer r2.bot.mu.Unlock()
		return r2.bot.session != nil
	})
	sentBeforePair := len(r2.sentTo(ownerID))
	r2.f.Text(t, "m-pair-2", ownerID, code)
	apitest.Eventually(t, "re-paired", func() bool { return len(r2.sentTo(ownerID)) > sentBeforePair })
	beforeSecond := len(r2.sentTo(ownerID))
	if _, err := p2.channel(context.Background(), "send", raw2); err != nil {
		t.Fatal(err)
	}
	if got := len(r2.sentTo(ownerID)) - beforeSecond; got != 1 {
		t.Fatalf("after logout and re-pair, reusing ref delivered %d messages, want 1", got)
	}
}

func TestHardeningChunkNonPositiveAndSanitizeFileName(t *testing.T) {
	if got := Chunk("hello", 0); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("Chunk(\"hello\", 0) = %v", got)
	}
	if got := Chunk("hello", -10); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("Chunk(\"hello\", -10) = %v", got)
	}

	r := newRig(t)
	r.pair()
	docURL, docKey := r.f.Media("doc-traversal", []byte("hello world"))
	r.f.Message(t, "pf1", ownerID, map[string]any{
		"msgtype": "file",
		"file": map[string]string{
			"url":      docURL,
			"aeskey":   docKey,
			"filename": "../../etc/evil\r\nname.txt",
		},
	})
	apitest.Eventually(t, "sanitized file", func() bool { return len(r.hub.messages()) == 1 })
	got := r.hub.messages()[0]
	if len(got.Files) != 1 || got.Files[0].Name != "evilname.txt" {
		t.Fatalf("file name was not sanitized: got %+v", got.Files)
	}
}

func TestHardeningConcurrentPauseRefcount(t *testing.T) {
	var inSubscribe atomic.Int32
	gate := make(chan struct{})
	firstEntered := make(chan struct{}, 1)
	bot := New(Options{
		NewClient: func(id, secret string) (*api.Client, error) {
			if secret == "slow" {
				select {
				case firstEntered <- struct{}{}:
				default:
				}
				inSubscribe.Add(1)
				<-gate
				return nil, errors.New("slow failed")
			}
			return nil, errors.New("fast failed")
		},
	}, &memSecrets{})
	bot.mu.Lock()
	bot.loaded = true
	bot.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = bot.LoginStep(context.Background(), stepCreds, map[string]string{"bot_id": "b1", "secret": "slow"})
	}()
	<-firstEntered

	// Second concurrent LoginStep fails quickly; bot must remain paused until the first one finishes too.
	_, _ = bot.LoginStep(context.Background(), stepCreds, map[string]string{"bot_id": "b1", "secret": "fast"})

	bot.mu.Lock()
	stillPaused := bot.paused
	bot.mu.Unlock()
	close(gate)
	wg.Wait()

	if !stillPaused {
		t.Fatal("fast concurrent LoginStep cleared pause while slow LoginStep was still running")
	}
	bot.mu.Lock()
	afterPaused := bot.paused
	bot.mu.Unlock()
	if afterPaused {
		t.Fatal("bot remained paused after all LoginStep calls finished")
	}
}
