package telegram

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/nwverse/persona-extensions/extensions/telegram/internal/connector"
)

func TestHardeningMemSessionConcurrentRace(t *testing.T) {
	m := &memSession{}
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for range 20 {
				_ = m.StoreSession(context.Background(), []byte{byte(id), 1, 2, 3})
				_, _ = m.LoadSession(context.Background())
			}
		}(i)
	}
	wg.Wait()
}

func TestHardeningLogoutClearsBoxAndPreventsSessionResurrection(t *testing.T) {
	c, _, ctx := archiveAccount(t)
	if _, _, _, err := c.Changes(ctx, "", 20); err != nil {
		t.Fatal(err)
	}
	c.keepPeers(map[string]peer{"u1": {Kind: 'u', ID: 1, Hash: 11}})
	c.keepLoc("m.u1.2.f", &fileLoc{key: "k1"})
	c.keepPhotoDCs(map[int64]int{99: 4})

	things, err := c.Collect(ctx)
	if err != nil || len(things) == 0 {
		t.Fatalf("expected populated inbox before logout, got %+v err=%v", things, err)
	}

	if err := c.Logout(ctx); err != nil {
		t.Fatal(err)
	}

	// After Logout, Collect and cached box state must be completely cleared.
	afterThings, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterThings) != 0 {
		t.Fatalf("Collect still returned previous account's inbox items after Logout: %+v", afterThings)
	}
	if len(c.listed()) != 0 {
		t.Fatalf("listed() still retained previous account dialogs after Logout: %+v", c.listed())
	}

	// A late gotd StoreSession flush after Logout must NOT resurrect secrets.
	if err := (sessionOf{c}).StoreSession(ctx, []byte("late-gotd-session")); err != nil {
		t.Fatal(err)
	}
	if raw, _ := c.secrets.Load(ctx); len(raw) != 0 {
		t.Fatalf("late StoreSession resurrected secrets after Logout: %s", string(raw))
	}
}

func TestHardeningChangesNonPositiveLimitClipAndNegativeMediaRefs(t *testing.T) {
	c, _, ctx := archiveAccount(t)
	batches, _, more, err := c.Changes(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) == 0 && more {
		t.Fatal("Changes with limit=0 returned 0 batches with more=true (infinite pagination loop)")
	}

	if got := clip("abcdef", 0); got != "" {
		t.Fatalf("clip(\"abcdef\", 0) = %q, want empty", got)
	}
	if got := clip("abcdef", -2); got != "" {
		t.Fatalf("clip(\"abcdef\", -2) = %q, want empty", got)
	}

	// Negative or zero photo/message IDs in media refs must be rejected before hitting Telegram.
	for _, ref := range []string{"a.u1.0", "a.u1.-5", "m.u1.0.f", "m.u1.-10.t"} {
		if _, err := c.Media(ctx, ref); err == nil {
			t.Errorf("Media(%q) succeeded, want error", ref)
		}
	}
}

type closingInvoker struct {
	closed atomic.Bool
}

func (c *closingInvoker) Invoke(context.Context, bin.Encoder, bin.Decoder) error { return nil }
func (c *closingInvoker) Close() error {
	c.closed.Store(true)
	return nil
}

func TestHardeningDCPoolsCloseWhileOpeningDoesNotLeak(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	inv := &closingInvoker{}

	pools := &dcPools{
		gate: newGate(),
		open: func(context.Context, int) (tg.Invoker, error) {
			close(started)
			<-release
			return inv, nil
		},
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := pools.get(context.Background(), 4)
		errCh <- err
	}()
	<-started

	pools.close()
	close(release)

	err := <-errCh
	if err == nil {
		t.Fatal("expected error from dcPools.get when pool was closed while opening")
	}
	if !inv.closed.Load() {
		t.Fatal("invoker opened concurrently with dcPools.close was leaked without Close()")
	}
}

func TestHardeningProxyConnectTunnelCRLFAndCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept connections and stall forever without replying.
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				time.Sleep(5 * time.Second)
			}(conn)
		}
	}()

	pu, _ := url.Parse("http://" + ln.Addr().String())
	var d net.Dialer

	// 1. Reject CRLF injection in addr.
	if _, err := connectTunnel(context.Background(), &d, pu, "1.2.3.4:443\r\nX-Injected: 1"); err == nil {
		t.Fatal("connectTunnel accepted CRLF in target addr")
	}

	// 2. Cancelable ctx (no deadline) must abort stalling proxy handshake promptly.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, cerr := connectTunnel(ctx, &d, pu, "1.2.3.4:443")
		done <- cerr
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case cerr := <-done:
		if cerr == nil {
			t.Fatal("expected cancel error from connectTunnel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connectTunnel hung ignoring context cancellation")
	}
}

func TestHardeningLateCheckSignedInAfterLogoutDoesNotClobberStatus(t *testing.T) {
	meStarted := make(chan struct{}, 1)
	meRelease := make(chan struct{})
	raw, _ := json.Marshal(stored{APIID: 1234567, APIHash: exampleHash, Session: []byte("example session")})
	sec := &memSecrets{data: raw}

	f := &fakeTelegram{signedIn: true}
	c := newConn(sec, func(ctx context.Context, cr creds, store sessionStore, g *gate, updated func()) (client, error) {
		cl, err := f.dial(ctx, cr, store, g, updated)
		if err != nil {
			return nil, err
		}
		return &slowMeClient{client: cl, started: meStarted, release: meRelease}, nil
	})
	c.gate.pace = func() time.Duration { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	select {
	case <-meStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("Me never started")
	}

	// Clear credentials directly as Logout does (simulating Logout winning before Me returns).
	c.mu.Lock()
	c.st = stored{}
	c.cl = nil
	_ = c.saveLocked(ctx)
	c.status = connector.Status{State: connector.StateNotSignedIn, Error: "还没填 Telegram 的 API 凭据"}
	c.mu.Unlock()

	close(meRelease)
	time.Sleep(80 * time.Millisecond)

	if st := c.Status(ctx); st.State != connector.StateNotSignedIn {
		t.Fatalf("late checkSignedIn overwrote signed-out status: %+v", st)
	}
	if data, _ := sec.Load(ctx); len(data) != 0 {
		t.Fatalf("late checkSignedIn resurrected secrets: %s", string(data))
	}
}

type slowMeClient struct {
	client
	started chan struct{}
	release chan struct{}
}

func (s *slowMeClient) Me(ctx context.Context) (string, bool, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	return "Alice", true, nil
}
