package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	qrStep        = "scan"
	secretVersion = 1
	refreshLead   = 30 * time.Second
	maxJWTExpiry  = 253402300799 // Last Unix second representable in an RFC 3339 timestamp.
)

type steamSecret struct {
	Version      int       `json:"version"`
	SteamID      string    `json:"steam_id"`
	Account      string    `json:"account_name"`
	RefreshToken string    `json:"refresh_token"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type steamTokens struct {
	secret        steamSecret
	refreshExpiry time.Time
	access        string
	accessExpiry  time.Time
}

type qrLogin struct {
	ctx       context.Context
	cancel    context.CancelFunc
	ready     chan struct{}
	expiresAt time.Time
	challenge qrChallenge // guarded by qrAuth.mu
}

type qrSession struct {
	auth       *qrAuth
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	tokens     steamTokens
	reads      *steamData
	rotation   *steamTokens
	refresh    *authResultWait
}

type authResultWait struct {
	done  chan struct{}
	issue problem // set before closing done
}

type qrAuth struct {
	ctx    context.Context
	hub    *ext.Hub
	client steamClient
	now    func() time.Time
	config config
	locale string

	loaded     chan struct{}
	cancelLoad context.CancelFunc

	mu         sync.Mutex
	generation uint64
	login      *qrLogin
	session    *qrSession
	clearing   *authResultWait
	issue      problem

	// Serialize secret mutations while leaving status and the local sign-out gate responsive.
	storeMu sync.Mutex
}

func newQRAuth(ctx context.Context, hub *ext.Hub, client *http.Client, now func() time.Time, cfg config, locale string) *qrAuth {
	loadCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	a := &qrAuth{ctx: ctx, hub: hub, client: steamClient{http: client}, now: now, config: cfg, locale: locale,
		loaded: make(chan struct{}), cancelLoad: cancel, issue: problemAuthRequired}
	go a.restore(loadCtx)
	return a
}

func (a *app) Status(ctx context.Context) (wire.AuthStatus, error) {
	t := textFor(a.locale)
	if a.qr != nil {
		if !a.qr.waitLoaded(ctx) {
			return wire.AuthStatus{State: wire.AuthStateError, Message: t.message(problemAuthLoad, false)}, nil
		}
		return a.qr.status(t), nil
	}
	if issue := a.keyStatusIssue(); issue != "" {
		return wire.AuthStatus{State: wire.AuthStateError, Message: t.message(issue, false)}, nil
	}
	return wire.AuthStatus{State: wire.AuthStateSignedIn, Message: t.keyMode}, nil
}

func (a *app) Step(ctx context.Context, step string, _ map[string]string) (wire.AuthPrompt, error) {
	t := textFor(a.locale)
	if a.qr == nil {
		if a.config.AuthMode == modeWebAPIKey {
			return wire.AuthPrompt{Error: t.keyMode}, nil
		}
		return wire.AuthPrompt{Error: t.message(a.setup, false)}, nil
	}
	if step != "" && step != qrStep {
		return wire.AuthPrompt{Error: t.message(problemAuthRefused, false)}, nil
	}
	return a.qr.step(ctx, step, t), nil
}

func (a *app) Logout(ctx context.Context) (wire.AuthStatus, error) {
	t := textFor(a.locale)
	if a.qr == nil {
		if a.config.AuthMode == modeWebAPIKey {
			return wire.AuthStatus{}, ext.Unsupported(t.keySignOut)
		}
		return a.Status(ctx)
	}
	if issue := a.qr.logout(ctx); issue != "" {
		return wire.AuthStatus{}, &ext.Error{Code: string(issue), Message: t.message(issue, false)}
	}
	return wire.AuthStatus{State: wire.AuthStateSignedOut, Message: t.message(problemAuthRequired, false)}, nil
}

func (a *qrAuth) restore(ctx context.Context) {
	defer close(a.loaded)
	defer a.cancelLoad()
	raw, err := a.hub.LoadSecret(ctx, extensionID)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.generation != 0 {
		return
	}
	if err != nil {
		a.issue = problemAuthLoad
		return
	}
	if len(raw) == 0 {
		return
	}
	var saved steamSecret
	if len(raw) > maxAuthBytes || json.Unmarshal(raw, &saved) != nil || saved.Version != secretVersion {
		a.issue = problemAuthLoad
		return
	}
	id, expiry, ok := steamJWT(saved.RefreshToken)
	if !ok || id != saved.SteamID {
		a.issue = problemAuthLoad
		return
	}
	if !expiry.After(a.now()) {
		a.clearLocked(problemAuthExpired)
		return
	}
	saved.Account = cleanText(redactCredentials(saved.Account, saved.RefreshToken), nameRunes)
	a.installLocked(steamTokens{secret: saved, refreshExpiry: expiry})
}

func (a *qrAuth) waitLoaded(ctx context.Context) bool {
	select {
	case <-a.loaded:
		return true
	case <-ctx.Done():
		return false
	}
}

func (a *qrAuth) status(t *text) wire.AuthStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessionLocked(); s != nil {
		return wire.AuthStatus{State: wire.AuthStateSignedIn, Account: s.tokens.account(), ExpiresAt: s.tokens.refreshExpiry}
	}
	if a.login != nil {
		return wire.AuthStatus{State: wire.AuthStatePending, ExpiresAt: a.login.expiresAt}
	}
	state := wire.AuthStateError
	switch a.issue {
	case problemAuthRequired:
		state = wire.AuthStateSignedOut
	case problemAuthExpired:
		state = wire.AuthStateExpired
	}
	return wire.AuthStatus{State: state, Message: t.message(a.issue, false)}
}

func (a *qrAuth) step(ctx context.Context, step string, t *text) wire.AuthPrompt {
	if !a.waitLoaded(ctx) {
		return wire.AuthPrompt{Error: t.message(problemAuthLoad, false)}
	}
	a.mu.Lock()
	if a.sessionLocked() == nil && a.login == nil && a.clearing == nil && step == "" {
		loginCtx, cancel := context.WithTimeout(a.ctx, qrLifetime)
		a.login = &qrLogin{ctx: loginCtx, cancel: cancel, ready: make(chan struct{}), expiresAt: a.now().Add(qrLifetime)}
		a.issue = problemAuthTimeout
		go a.runLogin(a.login)
	}
	l := a.login
	a.mu.Unlock()
	if l != nil {
		select {
		case <-l.ready:
		case <-l.ctx.Done():
		case <-ctx.Done():
			return wire.AuthPrompt{Error: t.message(sanitizeError(ctx.Err()), false)}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.sessionLocked() != nil:
		return wire.AuthPrompt{Done: true, Account: a.session.tokens.account()}
	case a.clearing != nil:
		return wire.AuthPrompt{Error: t.message(problemAuthClearing, false)}
	case a.login == l && l != nil && l.challenge.url != "" && l.ctx.Err() == nil:
		return wire.AuthPrompt{Step: qrStep, Kind: wire.ConnectorPromptKindQr, URL: l.challenge.url,
			Hint: t.qrHint, Interval: int(l.challenge.interval.Seconds()), ExpiresAt: l.expiresAt}
	default:
		return wire.AuthPrompt{Error: t.message(a.issue, false)}
	}
}

func (a *qrAuth) runLogin(l *qrLogin) {
	defer l.cancel()
	challenge, issue := a.client.beginQR(l.ctx)
	if issue != "" {
		a.endLogin(l, issue)
		close(l.ready)
		return
	}
	a.mu.Lock()
	if a.login != l {
		close(l.ready)
		a.mu.Unlock()
		return
	}
	l.challenge = challenge
	close(l.ready)
	a.mu.Unlock()
	for waitPoll(l.ctx, challenge.interval) {
		grant, issue := a.client.pollQR(l.ctx, challenge)
		if l.ctx.Err() != nil {
			break
		}
		switch issue {
		case "":
		case problemAuthPending, problemRateLimited, problemNetwork, problemTimeout:
			continue
		default:
			a.endLogin(l, problemAuthRefused)
			return
		}
		if grant.clientID != 0 || grant.url != "" {
			if grant.clientID != 0 {
				challenge.clientID = grant.clientID
			}
			if grant.url != "" {
				challenge.url = grant.url
			}
			a.mu.Lock()
			if a.login == l {
				l.challenge = challenge
			}
			a.mu.Unlock()
		}
		if grant.refresh == "" && grant.access == "" && !grant.agreement {
			continue
		}
		tokens, ok := loginTokens(grant, challenge, a.now())
		if !ok {
			a.endLogin(l, problemAuthRefused)
			return
		}
		if issue := a.completeLogin(l, tokens); issue != "" {
			a.endLogin(l, issue)
		}
		return
	}
	a.endLogin(l, problemAuthTimeout)
}

func waitPoll(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func (a *qrAuth) endLogin(l *qrLogin, issue problem) {
	a.mu.Lock()
	if a.login != l {
		a.mu.Unlock()
		return
	}
	a.login, a.issue = nil, issue
	l.cancel()
	a.mu.Unlock()
	a.changed()
}

func loginTokens(g qrGrant, challenge qrChallenge, now time.Time) (steamTokens, bool) {
	id, expiry, ok := steamJWT(g.refresh)
	if !ok || !expiry.After(now) {
		return steamTokens{}, false
	}
	account := redactCredentials(g.account, g.refresh, g.access, string(challenge.requestID),
		base64.StdEncoding.EncodeToString(challenge.requestID))
	t := steamTokens{secret: steamSecret{Version: secretVersion, SteamID: id, Account: cleanText(account, nameRunes),
		RefreshToken: g.refresh, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, refreshExpiry: expiry}
	if g.access != "" && !t.setAccess(g.access, now) {
		return steamTokens{}, false
	}
	return t, true
}

func (t steamTokens) account() string {
	if t.secret.Account != "" && t.secret.Account != "[redacted]" {
		return t.secret.Account
	}
	return t.secret.SteamID
}

func (t *steamTokens) setAccess(access string, now time.Time) bool {
	id, expiry, ok := steamJWT(access)
	if !ok || id != t.secret.SteamID || !expiry.After(now) {
		return false
	}
	t.access, t.accessExpiry = access, expiry
	return true
}

// JWTs are issued by Valve over HTTPS and restored only from the Hub's sealed
// store. These claims scope the session and schedule renewal.
func steamJWT(token string) (string, time.Time, bool) {
	header, rest, ok := strings.Cut(token, ".")
	if !ok || header == "" || len(token) > maxAuthBytes/4 {
		return "", time.Time{}, false
	}
	payload, signature, ok := strings.Cut(rest, ".")
	if !ok || signature == "" || strings.Contains(signature, ".") {
		return "", time.Time{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	var claims struct {
		Subject string `json:"sub"`
		Expiry  int64  `json:"exp"`
	}
	if err != nil || json.Unmarshal(data, &claims) != nil || !validSteamID(claims.Subject) || claims.Expiry <= 0 || claims.Expiry > maxJWTExpiry {
		return "", time.Time{}, false
	}
	return claims.Subject, time.Unix(claims.Expiry, 0).UTC(), true
}

func (a *qrAuth) completeLogin(l *qrLogin, t steamTokens) problem {
	a.storeMu.Lock()
	defer a.storeMu.Unlock()
	a.mu.Lock()
	active := a.login == l && l.ctx.Err() == nil
	a.mu.Unlock()
	if !active {
		return problemAuthTimeout
	}
	if issue := a.save(t.secret); issue != "" {
		return issue
	}
	a.mu.Lock()
	if a.login != l {
		a.mu.Unlock()
		return problemCanceled
	}
	a.installLocked(t)
	a.login = nil
	l.cancel()
	a.mu.Unlock()
	a.changed()
	return ""
}

func (a *qrAuth) save(secret steamSecret) problem {
	raw, err := json.Marshal(secret)
	// Use the process context so cancellation of a poll/read cannot abandon a
	// save before the Hub answers it. A subsequent delete follows that answer.
	if err != nil || a.hub.SaveSecret(a.ctx, extensionID, raw) != nil {
		return problemAuthSave
	}
	return ""
}

func (a *qrAuth) installLocked(t steamTokens) {
	a.generation++
	ctx, cancel := context.WithCancel(a.ctx)
	s := &qrSession{auth: a, generation: a.generation, ctx: ctx, cancel: cancel, tokens: t}
	cfg := a.config
	cfg.SteamID = t.secret.SteamID
	s.reads = newAccountData(cfg, a.client, "", a.now, s)
	a.session = s
	a.issue = ""
	s.reads.startWishlistInbox(s.ctx, a.hub, textFor(a.locale))
}

func (a *qrAuth) sessionLocked() *qrSession {
	s := a.session
	if s != nil && s.refresh == nil {
		if !s.refreshExpiryLocked().After(a.now()) {
			a.clearLocked(problemAuthExpired)
			return nil
		}
	}
	return s
}

func (s *qrSession) refreshExpiryLocked() time.Time {
	if s.rotation != nil {
		return s.rotation.refreshExpiry
	}
	return s.tokens.refreshExpiry
}

func (s *qrSession) currentLocked() bool {
	a := s.auth
	return a.session == s && a.generation == s.generation && s.ctx.Err() == nil
}

func (s *qrSession) access(ctx context.Context) (steamTokens, problem) {
	if err := ctx.Err(); err != nil {
		return steamTokens{}, sanitizeError(err)
	}
	a := s.auth
	a.mu.Lock()
	if !s.currentLocked() || a.sessionLocked() != s {
		a.mu.Unlock()
		return steamTokens{}, problemCanceled
	}
	if s.rotation == nil && s.tokens.accessExpiry.After(a.now().Add(refreshLead)) {
		t := s.tokens
		a.mu.Unlock()
		return t, ""
	}
	if s.refresh == nil {
		s.refresh = &authResultWait{done: make(chan struct{})}
		go s.refreshTokens(s.refresh)
	}
	refresh := s.refresh
	a.mu.Unlock()
	select {
	case <-refresh.done:
	case <-ctx.Done():
		return steamTokens{}, sanitizeError(ctx.Err())
	case <-s.ctx.Done():
		return steamTokens{}, problemCanceled
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !s.currentLocked() {
		return steamTokens{}, problemCanceled
	}
	if refresh.issue != "" {
		return steamTokens{}, refresh.issue
	}
	if !s.tokens.accessExpiry.After(a.now()) {
		return steamTokens{}, problemTimeout
	}
	return s.tokens, ""
}

func (s *qrSession) refreshTokens(wait *authResultWait) {
	issue := s.renew()
	a := s.auth
	a.mu.Lock()
	if issue == problemAuthExpired && s.currentLocked() {
		a.clearLocked(problemAuthExpired)
	}
	s.refresh = nil
	wait.issue = issue
	close(wait.done)
	a.mu.Unlock()
}

func (s *qrSession) renew() problem {
	a := s.auth
	a.mu.Lock()
	t, rotation := s.tokens, s.rotation
	current := s.currentLocked()
	a.mu.Unlock()
	if !current {
		return problemCanceled
	}
	rotate := rotation != nil
	if rotate {
		t = *rotation
	} else {
		g, issue := a.client.renewAccess(s.ctx, t.secret.RefreshToken, t.secret.SteamID)
		if issue != "" {
			return issue
		}
		now := a.now()
		if !t.setAccess(g.access, now) {
			return problemResponse
		}
		if g.refresh != "" && g.refresh != t.secret.RefreshToken {
			id, expiry, ok := steamJWT(g.refresh)
			if !ok || id != t.secret.SteamID || !expiry.After(now) {
				return problemResponse
			}
			t.secret.RefreshToken, t.secret.UpdatedAt, t.refreshExpiry = g.refresh, now.UTC(), expiry
			rotate = true
		}
	}
	if !t.refreshExpiry.After(a.now()) {
		return problemAuthExpired
	}
	if rotate {
		a.storeMu.Lock()
		defer a.storeMu.Unlock()
		a.mu.Lock()
		if !s.currentLocked() {
			a.mu.Unlock()
			return problemCanceled
		}
		// Keep a validated rotation for a later save if the secret store is unavailable.
		s.rotation = &t
		a.mu.Unlock()
		if issue := a.save(t.secret); issue != "" {
			return issue
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !s.currentLocked() {
		return problemCanceled
	}
	s.tokens, s.rotation = t, nil
	return ""
}

func (a *qrAuth) clearLocked(issue problem) *authResultWait {
	a.generation++
	a.cancelLoad()
	if a.login != nil {
		a.login.cancel()
		a.login.challenge = qrChallenge{}
		a.login = nil
	}
	if s := a.session; s != nil {
		s.cancel()
		s.tokens, s.rotation = steamTokens{}, nil
		s.reads.clearLocked()
		a.session = nil
	}
	a.issue = issue
	done := &authResultWait{done: make(chan struct{})}
	a.clearing = done
	go a.deleteStored(done)
	return done
}

func (a *qrAuth) deleteStored(wait *authResultWait) {
	a.storeMu.Lock()
	err := a.hub.DeleteSecret(a.ctx, extensionID)
	a.storeMu.Unlock()
	a.mu.Lock()
	if err != nil {
		wait.issue = problemAuthDelete
		if a.issue != problemAuthExpired {
			a.issue = problemAuthDelete
		}
	}
	a.clearing = nil
	close(wait.done)
	a.mu.Unlock()
	a.changed()
}

func (a *qrAuth) logout(ctx context.Context) problem {
	a.mu.Lock()
	wait := a.clearing
	if wait == nil {
		wait = a.clearLocked(problemAuthRequired)
	}
	a.mu.Unlock()
	select {
	case <-wait.done:
		return wait.issue
	case <-ctx.Done():
		return sanitizeError(ctx.Err())
	}
}

func (a *qrAuth) changed() {
	_ = a.hub.AuthChanged()
	_ = a.hub.Invalidate("")
}

func (a *qrAuth) redact(value string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.session; s != nil {
		value = redactCredentials(value, s.tokens.access, s.tokens.secret.RefreshToken)
		if s.rotation != nil {
			value = redactCredentials(value, s.rotation.access, s.rotation.secret.RefreshToken)
		}
	}
	if l := a.login; l != nil {
		value = redactCredentials(value, string(l.challenge.requestID), base64.StdEncoding.EncodeToString(l.challenge.requestID))
	}
	return redactCredentials(value)
}
