package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestSendDoesNotFollowRedirectOrLeakAuthHeader(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	dsn, err := ParseDSN("http://secretpublickey@"+origin.Listener.Addr().String()+"/42", true)
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	env, err := Envelope(dsn, Event(wire.ErrorReport{ID: "1", Source: "hub", Kind: "exception"}, Options{Environment: "test", Client: "persona-sentry/0.1.4"}))
	if err != nil {
		t.Fatalf("Envelope: %v", err)
	}

	// Both default newApp().client and an externally supplied http.Client without CheckRedirect must refuse redirects.
	for name, cl := range map[string]*http.Client{
		"default_app_client": newApp(func(string, ...any) {}).client,
		"plain_http_client":  origin.Client(),
	} {
		err := Send(context.Background(), cl, dsn, "persona-sentry/0.1.4", env, time.Now())
		if err == nil {
			t.Errorf("%s: Send followed 307 redirect and succeeded", name)
		}
	}
	if leaked.Load() != 0 {
		t.Fatalf("redirect target received %d request(s); X-Sentry-Auth leaked!", leaked.Load())
	}
}

func TestParseDSNRejectsHeaderInjectionQueryFragmentAndZeroProject(t *testing.T) {
	bad := []string{
		"https://abc%0d%0aX-Evil:1@sentry.io/42",
		"https://abc,foo@sentry.io/42",
		"https://abc%20def@sentry.io/42",
		"https://abc@sentry.io/42?extra=1",
		"https://abc@sentry.io/42?",
		"https://abc@sentry.io/42#frag",
		"https://abc@sentry.io/0",
	}
	for _, raw := range bad {
		if d, err := ParseDSN(raw, false); err == nil {
			t.Errorf("ParseDSN(%q) unexpectedly succeeded: %+v", raw, d)
		}
	}
}

func TestConfigureNaNSampleRateAndEmptyDSNReset(t *testing.T) {
	a := newApp(func(string, ...any) {})
	a.rand = func() float64 { return 0.5 }
	dsn, err := ParseDSN("https://pubkey@sentry.io/42", false)
	if err != nil {
		t.Fatal(err)
	}
	nan := math.NaN()
	a.configure(dsn, Config{SampleRate: &nan, Environment: "   "})
	if !a.takes(wire.ErrorReport{Kind: wire.ErrorKindException}) {
		t.Fatal("SampleRate=NaN silently dropped exception reports")
	}
	if a.opts.Environment != "production" {
		t.Fatalf("blank environment got %q, want production", a.opts.Environment)
	}

	// Re-initializing with empty DSN must clear active DSN so takes() returns false.
	in := &ext.Init{
		InitializeParams: wire.InitializeParams{
			Config:  json.RawMessage(`{}`),
			Secrets: map[string]string{"dsn": ""},
		},
	}
	if _, err := a.init(context.Background(), in, nil); err != nil {
		t.Fatalf("empty dsn init failed: %v", err)
	}
	if a.takes(wire.ErrorReport{Kind: wire.ErrorKindException}) {
		t.Fatal("takes() still returned true after re-init with empty DSN")
	}
}

func TestLimitedUntilZeroNegativeHugeAndHTTPDate(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	// 0 or negative X-Sentry-Rate-Limits on a 200 OK must NOT trigger errLimited.
	for _, hdr := range []string{"0:error", "-10:error", "0:"} {
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"X-Sentry-Rate-Limits": {hdr}},
		}
		if until, ok := limitedUntil(resp, now); ok {
			t.Errorf("limitedUntil(%q) = (%v, true), want false", hdr, until)
		}
	}

	// Huge seconds must be capped without int64 overflow.
	hugeResp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"X-Sentry-Rate-Limits": {"999999999999:error"}},
	}
	until, ok := limitedUntil(hugeResp, now)
	if !ok || !until.After(now) || until.Sub(now) > 24*time.Hour {
		t.Fatalf("huge X-Sentry-Rate-Limits got (%v, %v), want capped within 24h", until, ok)
	}

	// Case-insensitive and whitespace-padded categories must match.
	caseResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Sentry-Rate-Limits": {"30: ERROR ; transaction:org"}},
	}
	if until, ok := limitedUntil(caseResp, now); !ok || until.Sub(now) != 30*time.Second {
		t.Fatalf("case-insensitive X-Sentry-Rate-Limits got (%v, %v), want 30s", until, ok)
	}

	// HTTP-date Retry-After on 429 must be parsed.
	target := now.Add(90 * time.Second)
	dateResp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": {target.Format(http.TimeFormat)}},
	}
	if until, ok := limitedUntil(dateResp, now); !ok || until.Sub(now) != 90*time.Second {
		t.Fatalf("HTTP-date Retry-After got (%v, %v), want 90s", until, ok)
	}
}
