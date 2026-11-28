package api

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientDoesNotFollowRedirectOrLeakToken(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			_, _ = w.Write([]byte(`{"accessToken":"secret-tok","expireIn":7200}`))
		case "/v1.0/robot/oToMessages/batchSend":
			http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
		case "/v1.0/robot/messageFiles/download":
			_, _ = w.Write([]byte(`{"downloadUrl":"` + "https://" + r.Host + `/file"}`))
		case "/file":
			http.Redirect(w, r, target.URL+"/steal-file", http.StatusTemporaryRedirect)
		}
	}))
	defer origin.Close()

	c, err := New(Options{ClientID: "id", ClientSecret: "sec", BaseURL: origin.URL, HTTP: origin.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SendMarkdown(context.Background(), "staff-1", "hi", "body"); err == nil {
		t.Fatal("SendMarkdown followed redirect and succeeded")
	}
	if _, err := c.Download(context.Background(), "dc1", 1024); err == nil {
		t.Fatal("Download followed redirect and succeeded")
	}
	if leaked.Load() != 0 {
		t.Fatalf("redirect target received %d request(s)", leaked.Load())
	}
}

func TestNewRejectsUserinfoQueryFragment(t *testing.T) {
	for _, raw := range []string{
		"https://user:pass@api.dingtalk.com",
		"https://api.dingtalk.com?x=1",
		"https://api.dingtalk.com?",
		"https://api.dingtalk.com#frag",
	} {
		if _, err := New(Options{ClientID: "id", ClientSecret: "sec", BaseURL: raw}); err == nil {
			t.Errorf("New(%q) succeeded, want error", raw)
		}
	}
}

func TestCallOnceRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"x":"` + strings.Repeat("a", 1<<20+64) + `"}`))
	}))
	defer srv.Close()

	c, err := New(Options{ClientID: "id", ClientSecret: "sec", BaseURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := c.callOnce(context.Background(), "/big", false, map[string]string{}, &out); err == nil {
		t.Fatal("callOnce silently truncated >1MiB response without returning error")
	}
}

func TestTokenClampsHugeExpireIn(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"accessToken":"tok","expireIn":9223372036854775807}`))
	}))
	defer srv.Close()

	c, err := New(Options{ClientID: "id", ClientSecret: "sec", BaseURL: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if !c.expires.After(now) || c.expires.Sub(now) > 24*time.Hour {
		t.Fatalf("c.expires = %v (now=%v), want clamped within 24h", c.expires, now)
	}
	_ = math.MaxInt64
}

func TestDownloadAndReplyNormalizeHostsAndRejectUserinfo(t *testing.T) {
	c, err := New(Options{ClientID: "id", ClientSecret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.downloadURL("https://bucket.OSS-CN-HANGZHOU.ALIYUNCS.COM./obj"); err != nil {
		t.Fatalf("expected uppercase/FQDN OSS host to pass, got %v", err)
	}
	if _, err := c.downloadURL("https://user:pass@oapi.dingtalk.com/obj"); err == nil {
		t.Fatal("downloadURL accepted userinfo")
	}
	if !c.webhookHost("OAPI.DINGTALK.COM.") {
		t.Fatal("webhookHost rejected uppercase/FQDN DingTalk host")
	}
	if err := c.Reply(context.Background(), "https://user:pass@oapi.dingtalk.com/robot/send", "hi"); err == nil {
		t.Fatal("Reply accepted userinfo")
	}
}

func TestClipNonPositiveBound(t *testing.T) {
	if got := clip("hello", 0); got != "" {
		t.Fatalf("clip(\"hello\", 0) = %q, want empty", got)
	}
	if got := clip("hello", -3); got != "" {
		t.Fatalf("clip(\"hello\", -3) = %q, want empty", got)
	}
}
