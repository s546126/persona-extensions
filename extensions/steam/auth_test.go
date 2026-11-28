package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	firstQR   = "https://s.team/q/1/synthetic-first"
	secondQR  = "https://s.team/q/1/synthetic-second"
	requestID = "synthetic-request-id"
)

func fixtureJWT(t *testing.T, id string, expiry time.Time, tag string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"sub": id, "exp": expiry.Unix()})
	if err != nil {
		t.Fatal("could not encode synthetic JWT")
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"RS256"}`)) + "." + encode(payload) + "." + encode([]byte(tag))
}

func fixtureSecret(t *testing.T, id string, now time.Time) steamSecret {
	return steamSecret{Version: secretVersion, SteamID: id, Account: "synthetic_login",
		RefreshToken: fixtureJWT(t, id, now.Add(time.Hour), "refresh"),
		CreatedAt:    now.UTC(), UpdatedAt: now.UTC()}
}

func beginFixture() []byte {
	b := protoVarint(nil, 1, 101)
	b = protoBytes(b, 2, []byte(firstQR))
	b = protoBytes(b, 3, []byte(requestID))
	return binary.LittleEndian.AppendUint32(append(b, 4<<3|5), math.Float32bits(1))
}

func grantFixture(refresh, access string) []byte {
	b := protoBytes(nil, 3, []byte(refresh))
	b = protoBytes(b, 4, []byte(access))
	return protoBytes(b, 6, []byte("synthetic_login"))
}

func refreshFixture(access, refresh string) []byte {
	b := protoBytes(nil, 1, []byte(access))
	if refresh != "" {
		b = protoBytes(b, 2, []byte(refresh))
	}
	return b
}

func authFixture(w http.ResponseWriter, code string, body []byte) {
	w.Header().Set("X-EResult", code)
	_, _ = w.Write(body)
}

func authState(t *testing.T, h *exttest.Host) wire.AuthStatus {
	t.Helper()
	var state wire.AuthStatus
	if err := h.Call("auth.status", ext.Empty{}, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func authStep(t *testing.T, h *exttest.Host, step string) wire.AuthPrompt {
	t.Helper()
	var prompt wire.AuthPrompt
	if err := h.Call("auth.step", wire.AuthStepParams{Step: step}, &prompt); err != nil {
		t.Fatal(err)
	}
	return prompt
}

func await(t *testing.T, limit time.Duration, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatal("timed out waiting for the Steam extension")
		}
	}
}

func hasEvent(h *exttest.Host, name string) bool {
	for _, event := range h.Events() {
		if event.Name == name {
			return true
		}
	}
	return false
}

func assertHidden(t *testing.T, output string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(output, s) {
			t.Fatal("a credential or QR identifier was reflected")
		}
	}
}

func TestAuthModeSelection(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	for _, tc := range []struct {
		name, raw, key, state string
	}{
		{"fresh install", "{}", "", wire.AuthStateSignedOut},
		{"ID alone", fmt.Sprintf(`{"steam_id":%q}`, fixtureID), "", wire.AuthStateSignedOut},
		{"key alone", "{}", f.key, wire.AuthStateSignedOut},
		{"legacy pair", fmt.Sprintf(`{"steam_id":%q}`, fixtureID), f.key, wire.AuthStateSignedIn},
		{"explicit QR ignores legacy settings", `{"auth_mode":"qr","steam_id":"invalid id"}`, f.key, wire.AuthStateSignedOut},
		{"explicit key needs ID", `{"auth_mode":"web_api_key"}`, f.key, wire.AuthStateError},
		{"explicit key needs secret", fmt.Sprintf(`{"auth_mode":"web_api_key","steam_id":%q}`, fixtureID), "", wire.AuthStateError},
		{"explicit key is ready", fmt.Sprintf(`{"auth_mode":"web_api_key","steam_id":%q}`, fixtureID), f.key, wire.AuthStateSignedIn},
		{"unknown mode", `{"auth_mode":"unknown"}`, "", wire.AuthStateError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := exttest.StartWith(t, manifest, exttest.Serve(t, program(f.http, time.Now)), func(p *wire.InitializeParams) {
				p.Config, p.Hub.Locale = json.RawMessage(tc.raw), "en"
				p.Secrets = map[string]string{keyName: tc.key}
			})
			if st := authState(t, h); st.State != tc.state {
				t.Fatalf("auth.state = %q, want %q", st.State, tc.state)
			}
			if tc.state == wire.AuthStateSignedIn {
				if prompt := authStep(t, h, ""); prompt.Done || prompt.URL != "" || prompt.Error != english.keyMode {
					t.Fatal("key mode should direct the user to its configuration fields")
				}
			}
		})
	}
	if f.hit(playerPath) != 0 {
		t.Fatal("authentication status should use local state")
	}
}

func TestQRSignInRotationAndLogout(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	refresh := fixtureJWT(t, fixtureID, now.Add(time.Hour), "login-refresh")
	access := fixtureJWT(t, fixtureID, now.Add(time.Minute), "login-access")
	var begins, polls atomic.Int32
	var mu sync.Mutex
	var last time.Time
	f := newAuthServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.URL.Path == authBasePath+beginQRPath+"/v1/" {
			begins.Add(1)
			mu.Lock()
			last = time.Now()
			mu.Unlock()
			authFixture(w, "1", beginFixture())
			return
		}
		if r.URL.Path != authBasePath+pollQRPath+"/v1/" {
			t.Error("unexpected outbound method during sign-in")
			return
		}
		n := polls.Add(1)
		mu.Lock()
		if time.Since(last) < 3*time.Second {
			t.Error("Steam polls did not wait for the minimum interval")
		}
		last = time.Now()
		mu.Unlock()
		clientID := uint64(101)
		if n == 3 {
			clientID = 202
		}
		want := protoBytes(protoVarint(nil, 1, clientID), 2, []byte(requestID))
		if string(want) != string(body) {
			t.Error("poll used the wrong request ID or client ID")
		}
		switch n {
		case 1:
			authFixture(w, "22", []byte("ignored synthetic response"))
		case 2:
			authFixture(w, "1", protoBytes(protoVarint(nil, 1, 202), 2, []byte(secondQR)))
		case 3:
			authFixture(w, "1", grantFixture(refresh, access))
		default:
			t.Error("polling continued after approval")
		}
	})
	h := exttest.Start(t, manifest, exttest.Serve(t, program(f, func() time.Time { return now })))
	if st := authState(t, h); st.State != wire.AuthStateSignedOut {
		t.Fatalf("cold auth state = %q", st.State)
	}
	var calls sync.WaitGroup
	for range 8 {
		calls.Go(func() {
			var p wire.AuthPrompt
			err := h.Call("auth.step", wire.AuthStepParams{Step: ""}, &p)
			if err != nil || p.Kind != wire.ConnectorPromptKindQr || p.URL != firstQR || p.Interval != 3 ||
				!p.ExpiresAt.Equal(now.Add(qrLifetime)) || p.Done {
				t.Error("the caller did not receive the shared QR prompt")
			}
		})
	}
	calls.Wait()
	for range 5 {
		if p := authStep(t, h, qrStep); p.URL != firstQR || p.Done {
			t.Fatal("the pending step did not reuse the current QR")
		}
	}
	if begins.Load() != 1 || authState(t, h).State != wire.AuthStatePending {
		t.Fatal("Hub clients started more than one Steam login")
	}
	await(t, 8*time.Second, func() bool { return authStep(t, h, qrStep).URL == secondQR })
	await(t, 5*time.Second, func() bool { return authStep(t, h, qrStep).Done })
	raw := h.Secret(extensionID)
	var saved steamSecret
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Version != secretVersion ||
		saved.SteamID != fixtureID || saved.RefreshToken != refresh || saved.Account != "synthetic_login" ||
		!saved.CreatedAt.Equal(now) || !saved.UpdatedAt.Equal(now) {
		t.Fatal("the completed prompt had no durable versioned session")
	}
	assertHidden(t, string(raw), access, firstQR, secondQR, requestID)
	await(t, time.Second, func() bool { return hasEvent(h, "auth.changed") && hasEvent(h, "ui.invalidate") })
	var st wire.AuthStatus
	if err := h.Call("auth.logout", ext.Empty{}, &st); err != nil || st.State != wire.AuthStateSignedOut ||
		len(h.Secret(extensionID)) > 0 {
		t.Fatal("logout did not remove the stored session")
	}
	if p := authStep(t, h, qrStep); p.Done || p.URL != "" {
		t.Fatal("logout left a completed or pending login available")
	}
}

func TestQRSignInWaitsForSecretStore(t *testing.T) {
	t.Parallel()
	now := time.Now()
	refresh, access := fixtureJWT(t, fixtureID, now.Add(time.Hour), "refresh"), fixtureJWT(t, fixtureID, now.Add(time.Hour), "access")
	f := newAuthServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path == authBasePath+beginQRPath+"/v1/" {
			authFixture(w, "1", beginFixture())
		} else {
			authFixture(w, "1", grantFixture(refresh, access))
		}
	})
	h := exttest.StartWith(t, manifest, exttest.Serve(t, program(f, time.Now)), func(p *wire.InitializeParams) { p.Hub.Locale = "en" })
	started, reject := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(reject) })
	t.Cleanup(release)
	h.Answer("connector.secrets.save", func(json.RawMessage) (any, error) {
		close(started)
		<-reject
		return nil, fmt.Errorf("synthetic store failure %s %s %s %s", refresh, access, requestID, firstQR)
	})
	authStep(t, h, "")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the Steam grant was not sent to the Hub")
	}
	if p := authStep(t, h, qrStep); p.Done || authState(t, h).State != wire.AuthStatePending {
		t.Fatal("the extension enabled the session before the Hub acknowledged the save")
	}
	contains(t, toolAnswer(t, h, "steam_library", "{}"), `"available":false`, string(problemAuthPending))
	release()
	await(t, time.Second, func() bool { return authState(t, h).State == wire.AuthStateError })
	p := authStep(t, h, qrStep)
	if p.Done || p.URL != "" || p.Error != english.message(problemAuthSave, false) {
		t.Fatal("a failed save should leave the login unavailable")
	}
	assertHidden(t, p.Error, refresh, access, firstQR, requestID)
	if p := authStep(t, h, ""); p.Kind != wire.ConnectorPromptKindQr {
		t.Fatal("a failed save blocked a new attempt")
	}
}

func TestQRPollTerminalResponse(t *testing.T) {
	t.Parallel()
	f := newAuthServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path == authBasePath+beginQRPath+"/v1/" {
			authFixture(w, "1", beginFixture())
			return
		}
		// HTTP 200 alone would look like success, even with a body.
		authFixture(w, "27", []byte(firstQR))
	})
	h := exttest.StartWith(t, manifest, exttest.Serve(t, program(f, time.Now)), func(p *wire.InitializeParams) { p.Hub.Locale = "en" })
	authStep(t, h, "")
	await(t, 5*time.Second, func() bool { return authState(t, h).State == wire.AuthStateError })
	p := authStep(t, h, qrStep)
	if p.Done || p.URL != "" || p.Error != english.message(problemAuthRefused, false) {
		t.Fatal("a terminal Steam response should stop the login")
	}
	if p := authStep(t, h, ""); p.URL != firstQR {
		t.Fatal("a terminal response blocked a new attempt")
	}
}

func testSecretHub(t *testing.T) (*exttest.Host, context.Context, *ext.Hub) {
	t.Helper()
	var root context.Context
	var hub *ext.Hub
	p := ext.Program{ID: extensionID, Version: version, Init: func(ctx context.Context, _ *ext.Init, h *ext.Hub) (map[string]any, error) {
		root, hub = ctx, h
		return nil, nil
	}}
	h := exttest.Start(t, manifest, exttest.Serve(t, p))
	return h, root, hub
}

func answerSaved(t *testing.T, h *exttest.Host, saved steamSecret) {
	t.Helper()
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal("could not encode synthetic secret")
	}
	h.Answer("connector.secrets.load", func(json.RawMessage) (any, error) {
		return wire.ConnectorSecret{Data: base64.StdEncoding.EncodeToString(raw)}, nil
	})
}

func restoredSession(t *testing.T, h *exttest.Host, ctx context.Context, hub *ext.Hub, c *http.Client, now func() time.Time, saved steamSecret) (*qrAuth, *qrSession) {
	t.Helper()
	answerSaved(t, h, saved)
	q := newQRAuth(ctx, hub, c, now, config{AuthMode: modeQR, CountryCode: "GB"}, "en")
	if !q.waitLoaded(t.Context()) {
		t.Fatal("stored QR session did not load")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.session == nil || q.session.tokens.access != "" || q.session.tokens.secret.SteamID != saved.SteamID {
		t.Fatal("cold restore should load only the matching refresh credential")
	}
	return q, q.session
}

func TestQRLoginLocalDeadline(t *testing.T) {
	h, root, hub := testSecretHub(t)
	f := newAuthServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path != authBasePath+beginQRPath+"/v1/" {
			t.Error("polled after the local login deadline")
		}
		authFixture(w, "1", beginFixture())
	})
	q := newQRAuth(root, hub, f, time.Now, config{AuthMode: modeQR, CountryCode: "US"}, "en")
	q.waitLoaded(t.Context())
	ctx, cancel := context.WithTimeout(root, time.Second)
	defer cancel()
	l := &qrLogin{ctx: ctx, cancel: cancel, ready: make(chan struct{}), expiresAt: time.Now().Add(time.Second)}
	q.mu.Lock()
	q.login = l
	q.mu.Unlock()
	q.runLogin(l)
	if st := q.status(&english); st.State != wire.AuthStateError || st.Message != english.message(problemAuthTimeout, false) {
		t.Fatal("the local deadline did not end the pending login")
	}
	if p := q.step(t.Context(), "", &english); p.URL != firstQR {
		t.Fatal("the deadline blocked a new attempt")
	}
	if issue := q.logout(t.Context()); issue != "" || h.Secret(extensionID) != nil {
		t.Fatal("logout should cancel the replacement attempt")
	}
}

func TestQRStepRechecksSessionExpiry(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	saved := fixtureSecret(t, fixtureID, now)
	var checks atomic.Int32
	clock := func() time.Time {
		if checks.Add(1) == 1 {
			return now
		}
		return now.Add(time.Hour)
	}
	h, root, hub := testSecretHub(t)
	q, _ := restoredSession(t, h, root, hub, nil, clock, saved)
	// The session is eligible at step entry and expires before the response.
	checks.Store(0)
	if p := q.step(t.Context(), qrStep, &english); p.Done || p.Error == "" || p.Account != "" {
		t.Fatal("auth.step reported completion for a session that expired during the call")
	}
	if st := q.status(&english); st.State != wire.AuthStateExpired {
		t.Fatalf("auth state = %q, want expired", st.State)
	}
}

func TestQRRefreshCoalesces(t *testing.T) {
	t.Parallel()
	h, root, hub := testSecretHub(t)
	now := time.Now().Truncate(time.Second)
	saved := fixtureSecret(t, fixtureID, now)
	access := fixtureJWT(t, fixtureID, now.Add(time.Hour), "renewed-access")
	renewed := fixtureJWT(t, fixtureID, now.Add(2*time.Hour), "renewed-refresh")
	var upstream atomic.Int32
	f := newAuthServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		upstream.Add(1)
		authFixture(w, "1", refreshFixture(access, renewed))
	})
	q, s := restoredSession(t, h, root, hub, f, func() time.Time { return now }, saved)
	if q.status(&english).State != wire.AuthStateSignedIn || upstream.Load() != 0 {
		t.Fatal("cold status contacted Steam")
	}
	var calls sync.WaitGroup
	for range 20 {
		calls.Go(func() {
			tokens, issue := s.access(t.Context())
			if issue != "" || tokens.access != access || tokens.secret.RefreshToken != renewed {
				t.Error("concurrent reads did not receive the saved rotation")
			}
		})
	}
	calls.Wait()
	raw := h.Secret(extensionID)
	var stored steamSecret
	if upstream.Load() != 1 || json.Unmarshal(raw, &stored) != nil || stored.RefreshToken != renewed {
		t.Fatal("refresh was not coalesced and persisted")
	}
	assertHidden(t, string(raw), access)
}

func TestQRRefreshBeforeAccessExpiry(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(now.Unix())
	saved := fixtureSecret(t, fixtureID, now)
	first := fixtureJWT(t, fixtureID, now.Add(time.Minute), "first-access")
	second := fixtureJWT(t, fixtureID, now.Add(10*time.Minute), "second-access")
	var calls atomic.Int32
	f := newAuthServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		access := first
		if calls.Add(1) > 1 {
			access = second
		}
		authFixture(w, "1", refreshFixture(access, ""))
	})
	h, root, hub := testSecretHub(t)
	_, s := restoredSession(t, h, root, hub, f, func() time.Time { return time.Unix(clock.Load(), 0) }, saved)
	for _, tc := range []struct {
		at    time.Duration
		token string
		calls int32
	}{
		{0, first, 1},
		{29 * time.Second, first, 1},
		{31 * time.Second, second, 2},
	} {
		clock.Store(now.Add(tc.at).Unix())
		got, issue := s.access(t.Context())
		if issue != "" || got.access != tc.token || calls.Load() != tc.calls || h.Secret(extensionID) != nil {
			t.Fatalf("wrong renewal behavior at %s, issue %q", tc.at, issue)
		}
	}
}

func TestQRLogoutOrdersDeleteAfterPendingSave(t *testing.T) {
	t.Parallel()
	now := time.Now()
	saved := fixtureSecret(t, fixtureID, now)
	access := fixtureJWT(t, fixtureID, now.Add(time.Hour), "renewed-access")
	renewed := fixtureJWT(t, fixtureID, now.Add(2*time.Hour), "renewed-refresh")
	f := newAuthServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		authFixture(w, "1", refreshFixture(access, renewed))
	})
	h, root, hub := testSecretHub(t)
	q, s := restoredSession(t, h, root, hub, f, func() time.Time { return now }, saved)
	saveStarted, releaseSave := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseSave) })
	t.Cleanup(release)
	var savedThenDeleted atomic.Int32
	h.Answer("connector.secrets.save", func(json.RawMessage) (any, error) {
		close(saveStarted)
		<-releaseSave
		savedThenDeleted.Store(1)
		return ext.Empty{}, nil
	})
	h.Answer("connector.secrets.delete", func(json.RawMessage) (any, error) {
		if !savedThenDeleted.CompareAndSwap(1, 2) {
			t.Error("delete overtook an in-flight save")
		}
		return ext.Empty{}, nil
	})
	read := make(chan problem, 1)
	go func() {
		tokens, issue := s.access(t.Context())
		if tokens.access != "" {
			t.Error("read used a rotation with an unacknowledged save")
		}
		read <- issue
	}()
	select {
	case <-saveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never tried to save")
	}
	logout := make(chan problem, 1)
	go func() { logout <- q.logout(t.Context()) }()
	await(t, time.Second, func() bool { return q.status(&english).State == wire.AuthStateSignedOut })
	select {
	case issue := <-read:
		if issue != problemCanceled {
			t.Fatalf("logout left a waiting read with issue %q", issue)
		}
	case <-time.After(time.Second):
		t.Fatal("logout did not cancel the waiting read")
	}
	if p := q.step(t.Context(), "", &english); p.Error != english.message(problemAuthClearing, false) || savedThenDeleted.Load() != 0 {
		t.Fatal("the read gate and a new login should be closed while deleting the old secret")
	}
	release()
	select {
	case issue := <-logout:
		if issue != "" || savedThenDeleted.Load() != 2 || q.status(&english).State != wire.AuthStateSignedOut {
			t.Fatal("logout did not delete the secret after the save or the old refresh was installed")
		}
	case <-time.After(time.Second):
		t.Fatal("logout did not finish after the saved credential was acknowledged")
	}
}

func TestQRRefreshFailures(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	saved := fixtureSecret(t, fixtureID, now)
	access := fixtureJWT(t, fixtureID, now.Add(time.Hour), "new-access")
	renewed := fixtureJWT(t, fixtureID, now.Add(2*time.Hour), "new-refresh")
	other := fixtureJWT(t, fixtureID2, now.Add(time.Hour), "other-subject")
	for _, tc := range []struct {
		name, code, access, refresh string
		issue                       problem
	}{
		{"access subject", "1", other, "", problemResponse},
		{"rotated subject", "1", access, other, problemResponse},
		{"save fails", "1", access, renewed, problemAuthSave},
		{"revoked", "26", "", "", problemCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, root, hub := testSecretHub(t)
			var requests, saves, deletes atomic.Int32
			f := newAuthServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				requests.Add(1)
				authFixture(w, tc.code, refreshFixture(tc.access, tc.refresh))
			})
			q, s := restoredSession(t, h, root, hub, f, func() time.Time { return now }, saved)
			h.Answer("connector.secrets.save", func(raw json.RawMessage) (any, error) {
				if saves.Add(1) == 1 {
					return nil, fmt.Errorf("synthetic store error %s", renewed)
				}
				return ext.Empty{}, nil
			})
			h.Answer("connector.secrets.delete", func(json.RawMessage) (any, error) {
				deletes.Add(1)
				return ext.Empty{}, nil
			})
			if got, issue := s.access(t.Context()); got.access != "" || issue != tc.issue {
				t.Fatalf("access should fail closed with %q, got %q", tc.issue, issue)
			}
			switch tc.name {
			case "revoked":
				await(t, time.Second, func() bool { return hasEvent(h, "auth.changed") })
				if q.status(&english).State != wire.AuthStateExpired || deletes.Load() != 1 {
					t.Fatal("revoked session was not removed")
				}
			case "save fails":
				q.mu.Lock()
				current := s.tokens.secret.RefreshToken
				q.mu.Unlock()
				if current != saved.RefreshToken || saves.Load() != 1 {
					t.Fatal("an unsaved refresh token replaced the stored one")
				}
				got, issue := s.access(t.Context())
				if issue != "" || got.access != access || got.secret.RefreshToken != renewed || saves.Load() != 2 || requests.Load() != 1 {
					t.Fatal("retry should persist the same validated rotation before using it")
				}
			default:
				if saves.Load() != 0 || q.status(&english).State != wire.AuthStateSignedIn {
					t.Fatal("a mismatched subject changed the original session")
				}
			}
		})
	}
}
