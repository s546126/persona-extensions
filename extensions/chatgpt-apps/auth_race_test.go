package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

// deadlineContext transitions deterministically to context.DeadlineExceeded when expire() is called.
type deadlineContext struct {
	context.Context
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func newDeadlineContext(parent context.Context) *deadlineContext {
	return &deadlineContext{Context: parent, done: make(chan struct{})}
}

func (c *deadlineContext) expire() {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = context.DeadlineExceeded
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *deadlineContext) Done() <-chan struct{} { return c.done }

func (c *deadlineContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return c.Context.Err()
}

func TestPollAfterLogoutDoesNotResurrectSession(t *testing.T) {
	f := newFakeChatGPT(t)
	atSave := make(chan struct{})
	releaseSave := make(chan struct{})
	pollDone := make(chan struct{})
	var saveOnce, exitOnce sync.Once
	t.Cleanup(func() {
		select {
		case <-releaseSave:
		default:
			close(releaseSave)
		}
	})

	h := startTuned(t, f, "en", func(a *App) {
		a.beforeCompleteLogin = func() {
			saveOnce.Do(func() { close(atSave) })
			<-releaseSave
		}
		a.afterPoll = func() {
			exitOnce.Do(func() { close(pollDone) })
		}
	})

	var p wire.AuthPrompt
	if err := h.call("auth.step", wire.AuthStepParams{Step: ""}, &p); err != nil {
		t.Fatalf("auth.step: %v", err)
	}
	f.approve()

	select {
	case <-atSave:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for poll to reach completeLogin")
	}

	// Invoke real auth.logout while poll() holds exchanged tokens just before completeLogin().
	var st wire.AuthStatus
	if err := h.call("auth.logout", struct{}{}, &st); err != nil {
		t.Fatalf("auth.logout: %v", err)
	}
	if st.State != wire.AuthStateSignedOut {
		t.Fatalf("logout status: %+v", st)
	}
	close(releaseSave)

	select {
	case <-pollDone:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not exit")
	}

	if tok := h.app.current(); tok != nil {
		t.Fatalf("canceled poll resurrected tokens in memory: %+v", tok)
	}
	if secret := h.host.Secret(connectorID); secret != nil {
		t.Fatalf("canceled poll persisted secret after logout: %q", secret)
	}
	if err := h.call("auth.status", struct{}{}, &st); err != nil {
		t.Fatalf("auth.status: %v", err)
	}
	if st.State != wire.AuthStateSignedOut {
		t.Fatalf("auth.status after logout: %+v, want signed_out", st)
	}
	for _, e := range h.host.Events() {
		if e.Name == "auth.changed" {
			t.Errorf("unexpected auth.changed event from canceled poll: %+v", e)
		}
	}
}

func TestCompleteLoginSerializesSaveSecretWithLogout(t *testing.T) {
	f := newFakeChatGPT(t)
	h := start(t, f, "en")

	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	var (
		mu    sync.Mutex
		order []string
	)
	var saveOnce sync.Once
	t.Cleanup(func() {
		select {
		case <-releaseSave:
		default:
			close(releaseSave)
		}
	})

	h.host.Answer("connector.secrets.save", func(_ json.RawMessage) (any, error) {
		mu.Lock()
		order = append(order, "save:start")
		mu.Unlock()
		saveOnce.Do(func() { close(saveStarted) })
		<-releaseSave
		mu.Lock()
		order = append(order, "save:end")
		mu.Unlock()
		return map[string]any{}, nil
	})
	h.host.Answer("connector.secrets.delete", func(_ json.RawMessage) (any, error) {
		mu.Lock()
		order = append(order, "delete")
		mu.Unlock()
		return map[string]any{}, nil
	})

	var p wire.AuthPrompt
	if err := h.call("auth.step", wire.AuthStepParams{Step: ""}, &p); err != nil {
		t.Fatalf("auth.step: %v", err)
	}
	f.approve()

	select {
	case <-saveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SaveSecret to start")
	}

	logoutDone := make(chan wire.AuthStatus, 1)
	logoutErr := make(chan error, 1)
	go func() {
		var st wire.AuthStatus
		err := h.call("auth.logout", struct{}{}, &st)
		logoutErr <- err
		logoutDone <- st
	}()

	// Logout must block on refreshMu until SaveSecret completes so DeleteSecret cannot overtake SaveSecret.
	select {
	case err := <-logoutErr:
		t.Fatalf("Logout returned while SaveSecret was still in flight (err=%v)", err)
	case <-time.After(80 * time.Millisecond):
	}
	mu.Lock()
	snapshot := slices.Clone(order)
	mu.Unlock()
	if !slices.Equal(snapshot, []string{"save:start"}) {
		t.Fatalf("secret ops while SaveSecret blocked = %v, want [save:start]", snapshot)
	}

	close(releaseSave)

	select {
	case err := <-logoutErr:
		if err != nil {
			t.Fatalf("auth.logout: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Logout did not finish after SaveSecret completed")
	}
	if st := <-logoutDone; st.State != wire.AuthStateSignedOut {
		t.Fatalf("logout status: %+v", st)
	}
	mu.Lock()
	finalOrder := slices.Clone(order)
	mu.Unlock()
	if !slices.Equal(finalOrder, []string{"save:start", "save:end", "delete"}) {
		t.Fatalf("secret op order = %v, want [save:start save:end delete]", finalOrder)
	}
	if tok := h.app.current(); tok != nil {
		t.Fatalf("tokens remained in memory after serialized Logout: %+v", tok)
	}
}

func TestLogoutDuringCodeExchangeDoesNotEmitSpuriousErrorOrSave(t *testing.T) {
	f := newFakeChatGPT(t)
	inExchange := make(chan struct{})
	releaseExchange := make(chan struct{})
	pollDone := make(chan struct{})
	var exchangeOnce, exitOnce sync.Once
	f.mu.Lock()
	f.onCodeExchange = func() {
		exchangeOnce.Do(func() { close(inExchange) })
		<-releaseExchange
	}
	f.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-releaseExchange:
		default:
			close(releaseExchange)
		}
	})

	h := startTuned(t, f, "en", func(a *App) {
		a.afterPoll = func() { exitOnce.Do(func() { close(pollDone) }) }
	})
	var p wire.AuthPrompt
	if err := h.call("auth.step", wire.AuthStepParams{Step: ""}, &p); err != nil {
		t.Fatalf("auth.step: %v", err)
	}
	f.approve()

	select {
	case <-inExchange:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for code exchange request")
	}

	var st wire.AuthStatus
	if err := h.call("auth.logout", struct{}{}, &st); err != nil {
		t.Fatalf("auth.logout: %v", err)
	}
	if st.State != wire.AuthStateSignedOut {
		t.Fatalf("logout status: %+v", st)
	}
	close(releaseExchange)

	select {
	case <-pollDone:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not exit after logout")
	}

	if tok := h.app.current(); tok != nil {
		t.Fatalf("tokens saved after logout: %+v", tok)
	}
	if secret := h.host.Secret(connectorID); secret != nil {
		t.Fatalf("secret saved after logout: %q", secret)
	}
	for _, e := range h.host.Events() {
		if e.Name == "auth.changed" {
			t.Errorf("unexpected auth.changed event after logout during code exchange: %+v", e)
		}
	}
	if strings.Contains(h.logs.String(), "code exchange refused") {
		t.Errorf("unexpected error log on logout cancellation:\n%s", h.logs.String())
	}
}

func TestSupersededLoginPollDoesNotOverwriteActiveLogin(t *testing.T) {
	f := newFakeChatGPT(t)
	atSave := make(chan struct{})
	releaseSave := make(chan struct{})
	poll1Done := make(chan struct{})
	var saveOnce, exitOnce sync.Once
	t.Cleanup(func() {
		select {
		case <-releaseSave:
		default:
			close(releaseSave)
		}
	})

	h := startTuned(t, f, "en", func(a *App) {
		a.beforeCompleteLogin = func() {
			var block bool
			saveOnce.Do(func() {
				block = true
				close(atSave)
			})
			if block {
				<-releaseSave
			}
		}
		a.afterPoll = func() {
			exitOnce.Do(func() { close(poll1Done) })
		}
	})

	var p1 wire.AuthPrompt
	if err := h.call("auth.step", wire.AuthStepParams{Step: ""}, &p1); err != nil {
		t.Fatalf("start l1: %v", err)
	}
	f.approve()

	select {
	case <-atSave:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for l1 to reach completeLogin")
	}

	// Unapprove before starting l2 so l2 stays pending while l1 finishes unwinding.
	f.mu.Lock()
	f.approved = false
	f.mu.Unlock()

	var p2 wire.AuthPrompt
	if err := h.call("auth.step", wire.AuthStepParams{Step: ""}, &p2); err != nil {
		t.Fatalf("start l2: %v", err)
	}
	close(releaseSave)

	select {
	case <-poll1Done:
	case <-time.After(5 * time.Second):
		t.Fatal("l1 poll did not exit")
	}

	if tok := h.app.current(); tok != nil {
		t.Fatalf("superseded login l1 saved tokens: %+v", tok)
	}
	if secret := h.host.Secret(connectorID); secret != nil {
		t.Fatalf("superseded login l1 wrote hub secret: %q", secret)
	}
	var st wire.AuthStatus
	if err := h.call("auth.status", struct{}{}, &st); err != nil {
		t.Fatalf("auth.status: %v", err)
	}
	if st.State != wire.AuthStatePending {
		t.Fatalf("auth.status: %+v, want pending for l2", st)
	}
	for _, e := range h.host.Events() {
		if e.Name == "auth.changed" {
			t.Errorf("unexpected auth.changed event from superseded l1 poll: %+v", e)
		}
	}
	// Cleanly stop l2's background poll before teardown.
	_ = h.call("auth.logout", struct{}{}, &st)
}

func TestDeadlineDuringCodeExchangeReportsCodeExpired(t *testing.T) {
	f := newFakeChatGPT(t)
	inExchange := make(chan struct{})
	releaseExchange := make(chan struct{})
	var once sync.Once
	f.mu.Lock()
	f.approved = true
	f.onCodeExchange = func() {
		once.Do(func() { close(inExchange) })
		<-releaseExchange
	}
	f.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-releaseExchange:
		default:
			close(releaseExchange)
		}
	})

	h := start(t, f, "en")
	pctx := newDeadlineContext(h.app.ctx)
	l := &deviceLogin{
		deviceAuthID: "dev_example",
		userCode:     "ABCD-EFGH",
		interval:     time.Millisecond,
		expires:      time.Now().Add(time.Minute),
		cancel:       func() {},
	}
	h.app.mu.Lock()
	h.app.login = l
	h.app.mu.Unlock()

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		h.app.poll(pctx, l)
	}()

	select {
	case <-inExchange:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for code exchange")
	}
	// Deterministically expire the device code while tokenRequest is in flight.
	pctx.expire()
	close(releaseExchange)

	select {
	case <-pollDone:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not exit after deadline")
	}

	var p wire.AuthPrompt
	if err := h.call("auth.step", wire.AuthStepParams{Step: "device"}, &p); err != nil {
		t.Fatalf("auth.step: %v", err)
	}
	if p.Error != english.authCodeExpired {
		t.Fatalf("prompt error = %q, want %q", p.Error, english.authCodeExpired)
	}
	var st wire.AuthStatus
	if err := h.call("auth.status", struct{}{}, &st); err != nil {
		t.Fatalf("auth.status: %v", err)
	}
	if st.State != wire.AuthStateSignedOut {
		t.Fatalf("auth.status after code expiry = %+v, want signed_out", st)
	}
	changed := false
	for _, e := range h.host.Events() {
		changed = changed || e.Name == "auth.changed"
	}
	if !changed {
		t.Error("expected auth.changed after code expiry")
	}
}

func TestDeadlineBeforeCompleteLoginReportsCodeExpired(t *testing.T) {
	f := newFakeChatGPT(t)
	f.approve()
	atSave := make(chan struct{})
	releaseSave := make(chan struct{})
	var saveOnce sync.Once
	t.Cleanup(func() {
		select {
		case <-releaseSave:
		default:
			close(releaseSave)
		}
	})

	h := startTuned(t, f, "en", func(a *App) {
		a.beforeCompleteLogin = func() {
			saveOnce.Do(func() { close(atSave) })
			<-releaseSave
		}
	})

	pctx := newDeadlineContext(h.app.ctx)
	l := &deviceLogin{
		deviceAuthID: "dev_example",
		userCode:     "ABCD-EFGH",
		interval:     time.Millisecond,
		expires:      time.Now().Add(time.Minute),
		cancel:       func() {},
	}
	h.app.mu.Lock()
	h.app.login = l
	h.app.mu.Unlock()

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		h.app.poll(pctx, l)
	}()

	select {
	case <-atSave:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for poll to reach completeLogin")
	}
	pctx.expire()
	close(releaseSave)

	select {
	case <-pollDone:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not exit")
	}

	if tok := h.app.current(); tok != nil {
		t.Fatalf("expired poll saved tokens: %+v", tok)
	}
	var p wire.AuthPrompt
	if err := h.call("auth.step", wire.AuthStepParams{Step: "device"}, &p); err != nil {
		t.Fatalf("auth.step: %v", err)
	}
	if p.Error != english.authCodeExpired {
		t.Fatalf("prompt error = %q, want %q", p.Error, english.authCodeExpired)
	}
	var st wire.AuthStatus
	if err := h.call("auth.status", struct{}{}, &st); err != nil {
		t.Fatalf("auth.status: %v", err)
	}
	if st.State != wire.AuthStateSignedOut {
		t.Fatalf("auth.status = %+v, want signed_out", st)
	}
}

func TestLogoutDeletesSecretEvenWhenCallerContextCanceled(t *testing.T) {
	f := newFakeChatGPT(t)
	h := start(t, f, "en")
	h.signIn()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	st, err := h.app.Logout(canceled)
	if err != nil || st.State != wire.AuthStateSignedOut {
		t.Fatalf("Logout(canceled) = %+v, %v", st, err)
	}
	if secret := h.host.Secret(connectorID); secret != nil {
		t.Fatalf("stored secret remained after Logout with canceled caller ctx: %q", secret)
	}
}

func TestLateLoadTokensDoesNotResurrectAfterLogout(t *testing.T) {
	f := newFakeChatGPT(t)
	h := start(t, f, "en")
	h.signIn()

	// app2 simulates startup where LoadSecret already read the previously saved secret
	// from the hub, but Logout() runs before loadTokens() acquires app2.mu to store it.
	app2 := newApp(f.endpoints())
	app2.loadTimeout = time.Millisecond
	if _, err := app2.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	app2.mu.Lock()
	app2.hub = h.app.hub
	app2.mu.Unlock()
	app2.loadTokens(context.Background())

	if tok := app2.current(); tok != nil {
		t.Fatalf("late loadTokens resurrected tokens after Logout: %+v", tok)
	}
}

func TestRefreshAfter401ReturnsExpiredWhenConcurrentRefreshSignedOut(t *testing.T) {
	f := newFakeChatGPT(t)
	h := start(t, f, "en")
	h.signIn()

	if _, err := h.tool("chatgpt_apps", map[string]any{}); err != nil {
		t.Fatalf("chatgpt_apps: %v", err)
	}
	if _, ok := h.app.cachedTool("example_mail.search_messages"); !ok {
		t.Fatal("expected tool cache to be populated")
	}
	used := h.app.current().AccessToken

	f.mu.Lock()
	f.refresh = "revoked-elsewhere"
	f.refuse401 = 1
	f.mu.Unlock()

	if _, err := h.tool("chatgpt_apps", map[string]any{"refresh": true}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired error, got: %v", err)
	}

	// A second concurrent request that received 401 with the same access token enters
	// refreshAfter401 after the first request's refreshLocked marked the session expired.
	if _, err := h.app.refreshAfter401(context.Background(), used); !errors.Is(err, errExpired) {
		t.Fatalf("refreshAfter401 error = %v, want errExpired", err)
	}
	if _, ok := h.app.cachedTool("example_mail.search_messages"); ok {
		t.Fatal("expected caches to be cleared when refresh refusal signs out")
	}

	// A stale completed device login from the expired session must not reappear as a device prompt.
	var p wire.AuthPrompt
	if err := h.call("auth.step", wire.AuthStepParams{Step: "device"}, &p); err != nil {
		t.Fatalf("auth.step: %v", err)
	}
	if p.Error != english.authCodeExpired || p.UserCode != "" {
		t.Fatalf("auth.step after refresh expiry = %+v, want error %q", p, english.authCodeExpired)
	}
}
