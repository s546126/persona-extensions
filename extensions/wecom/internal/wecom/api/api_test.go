package api_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wecom/internal/wecom/api"
	"github.com/nwverse/persona-extensions/extensions/wecom/internal/wecom/api/apitest"
)

func client(t *testing.T, f *apitest.Fake, secret string) *api.Client {
	t.Helper()
	c, err := api.New(f.Options(apitest.BotID, secret))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRefusesPlainWebSocketAndMissingCredentials(t *testing.T) {
	if _, err := api.New(api.Options{BotID: "a", Secret: "b", URL: "ws://example.com"}); err == nil {
		t.Error("a ws:// address was taken")
	}
	if _, err := api.New(api.Options{BotID: "a"}); err == nil {
		t.Error("a client without a secret was made")
	}
}

func TestSubscribeChecksCredentials(t *testing.T) {
	f := apitest.New(t)
	if err := client(t, f, apitest.Secret).Subscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := client(t, f, "wrong").Subscribe(context.Background())
	var e *api.Error
	if !api.Unauthorized(err) || !errors.As(err, &e) || e.Code != 853000 || e.Message != "invalid bot_id or secret" {
		t.Fatalf("a wrong secret: %v", err)
	}
	if strings.Contains(err.Error(), "192.0.2.1") {
		t.Errorf("the hub's address was kept: %v", err)
	}
}

func TestRunDeliversInOrderSendsAndHeartbeats(t *testing.T) {
	old := api.PingEvery
	api.PingEvery = 20 * time.Millisecond
	defer func() { api.PingEvery = old }()
	f := apitest.New(t)
	c := client(t, f, apitest.Secret)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan api.Message, 10)
	sessions := make(chan *api.Session, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, func(s *api.Session) { sessions <- s }, func(fr api.Frame) {
			if fr.Cmd == api.CmdMessage {
				m, _ := api.ParseMessage(fr.Body)
				got <- m
			}
		})
	}()
	f.WaitConnected(t)
	s := <-sessions
	f.Text(t, "m1", "user-1", "one")
	f.Text(t, "m2", "user-1", "two")
	for _, want := range []string{"one", "two"} {
		select {
		case m := <-got:
			if m.PlainText() != want || m.From.UserID != "user-1" || m.ChatType != api.ChatSingle {
				t.Fatalf("got %+v, want %q", m, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no message")
		}
	}
	if err := s.SendMarkdown(ctx, "user-1", "**hi**"); err != nil {
		t.Fatal(err)
	}
	if sent := f.Sent(); len(sent) != 1 || sent[0] != (apitest.Sent{UserID: "user-1", Text: "**hi**"}) {
		t.Fatalf("sent %+v", sent)
	}
	f.Set(func(x *apitest.Fake) { x.Refuse = 40096 })
	var e *api.Error
	if err := s.SendMarkdown(ctx, "user-1", "x"); !errors.As(err, &e) || e.Code != 40096 {
		t.Fatalf("a refused send: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // a few heartbeats
	f.Kick(t)
	select {
	case err := <-done:
		if !errors.Is(err, api.ErrKicked) {
			t.Fatalf("after being taken over: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end when taken over")
	}
}

func TestMessageParts(t *testing.T) {
	m, err := api.ParseMessage([]byte(`{"msgid":"m","chattype":"single","from":{"userid":"u"},"msgtype":"mixed",
		"mixed":{"msg_item":[{"msgtype":"text","text":{"content":"look "}},{"msgtype":"image","image":{"url":"https://x.qq.com/a","aeskey":"k"}},{"msgtype":"text","text":{"content":"here"}}]},
		"quote":{"msgtype":"text","text":{"content":"the quoted line"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	p := m.Parts()
	if len(p) != 3 || p[1].Kind != api.KindImage || p[1].AESKey != "k" || m.PlainText() != "look here" || m.QuoteText() != "the quoted line" {
		t.Fatalf("mixed: %+v / %q / %q", p, m.PlainText(), m.QuoteText())
	}
	v, _ := api.ParseMessage([]byte(`{"msgid":"v","msgtype":"voice","voice":{"content":"spoken words"}}`))
	if v.PlainText() != "spoken words" {
		t.Fatalf("voice: %q", v.PlainText())
	}
}

func TestDownloadDecryptsOverHTTPS(t *testing.T) {
	f := apitest.New(t)
	c := client(t, f, apitest.Secret)
	plain := []byte("synthetic picture bytes, long enough to span a few AES blocks......")
	url, key := f.MediaNamed("pic", "报告 2026/../final.pdf", plain)
	data, name, err := c.Download(context.Background(), url, key, 1<<20)
	if err != nil || !bytes.Equal(data, plain) || name != "final.pdf" {
		t.Fatalf("download: %q, %q, %v", data, name, err)
	}
	if _, _, err := c.Download(context.Background(), url, key, 8); !errors.Is(err, api.ErrTooLarge) {
		t.Errorf("an oversized download: %v", err)
	}
	if _, _, err := c.Download(context.Background(), "https://example.com/x", key, 1<<20); err == nil {
		t.Error("downloaded from a host it does not know")
	}
	other, _ := apitest.Key("other")
	if _, _, err := c.Download(context.Background(), url, other, 1<<20); err == nil {
		t.Error("the wrong key decrypted")
	}
}

func TestFileNameFromAnURLEncodedDisposition(t *testing.T) {
	f := apitest.New(t)
	c := client(t, f, apitest.Secret)
	// as WeCom sends a Chinese name: URL-encoded in the plain filename, a space as "+"
	url, key := f.MediaNamed("doc", "%E7%94%B3%E8%AF%B7%E8%A1%A8+2026.doc", []byte("synthetic"))
	if _, name, err := c.Download(context.Background(), url, key, 1<<20); err != nil || name != "申请表 2026.doc" {
		t.Fatalf("name %q, %v", name, err)
	}
	url, key = f.MediaNamed("odd", "100%.txt", []byte("synthetic"))
	if _, name, _ := c.Download(context.Background(), url, key, 1<<20); name != "100%.txt" {
		t.Errorf("a name that is not URL-encoded: %q", name)
	}
}

func TestDecryptRoundTripAndPadding(t *testing.T) {
	_, key := apitest.Key("k")
	for _, n := range []int{0, 1, 31, 32, 33, 100} {
		plain := bytes.Repeat([]byte{'a'}, n)
		got, err := api.Decrypt(api.Encrypt(plain, key), key)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%d bytes: %v", n, err)
		}
	}
	if _, err := api.Decrypt([]byte("not blocks"), key); err == nil {
		t.Error("partial blocks decrypted")
	}
}

// The established connection outlives the dial's context (it bounds the handshake and the subscription only): a
// message still arrives and a send is answered well after DialTimeout (raised in review of #314).
func TestConnectionOutlivesTheDialTimeout(t *testing.T) {
	f := apitest.New(t)
	o := f.Options(apitest.BotID, apitest.Secret)
	o.DialTimeout = 100 * time.Millisecond
	c, err := api.New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sessions := make(chan *api.Session, 1)
	got := make(chan string, 1)
	go func() {
		_ = c.Run(ctx, func(s *api.Session) { sessions <- s }, func(fr api.Frame) {
			m, _ := api.ParseMessage(fr.Body)
			got <- m.ID
		})
	}()
	f.WaitConnected(t)
	s := <-sessions
	time.Sleep(500 * time.Millisecond) // five dial timeouts
	f.Text(t, "late", "user-1", "still there?")
	select {
	case id := <-got:
		if id != "late" {
			t.Fatalf("got %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection did not outlive the dial timeout")
	}
	if err := s.SendMarkdown(ctx, "user-1", "yes"); err != nil {
		t.Fatalf("a send after the dial timeout: %v", err)
	}
}

// A request on a closed session never left: it is ErrNotSent (retryable), not an unknown outcome.
func TestRequestOnAClosedSessionIsNotSent(t *testing.T) {
	f := apitest.New(t)
	c := client(t, f, apitest.Secret)
	ctx, cancel := context.WithCancel(context.Background())
	sessions := make(chan *api.Session, 1)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, func(s *api.Session) { sessions <- s }, nil) }()
	f.WaitConnected(t)
	s := <-sessions
	cancel()
	<-done
	err := s.SendMarkdown(context.Background(), "user-1", "x")
	if !errors.Is(err, api.ErrNotSent) || errors.Is(err, api.ErrNoAnswer) {
		t.Fatalf("a send on a closed session: %v", err)
	}
}

// Media redirects must stay on HTTPS and the media hosts.
func TestMediaRedirectsAreChecked(t *testing.T) {
	f := apitest.New(t)
	c := client(t, f, apitest.Secret)
	plain := []byte("synthetic")
	url, key := f.Media("pic", plain)
	base := strings.Replace(url, "/media/pic", "", 1)
	if data, _, err := c.Download(context.Background(), base+"/redirect?to=/media/pic", key, 1<<20); err != nil || !bytes.Equal(data, plain) {
		t.Fatalf("a redirect on the same host: %q, %v", data, err)
	}
	for _, to := range []string{"https://example.com/x", "http://" + strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://") + "/media/pic"} {
		if _, _, err := c.Download(context.Background(), base+"/redirect?to="+to, key, 1<<20); err == nil {
			t.Errorf("followed a redirect to %s", to)
		}
	}
}

func TestRedactKeepsURLsOutOfLogs(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://ww-aibot-img.cos.ap-guangzhou.myqcloud.com/x?sign=secret", Err: io.ErrUnexpectedEOF}
	if got := api.Redact(err); strings.Contains(got, "secret") || !strings.Contains(got, "myqcloud.com") {
		t.Fatalf("redacted: %q", got)
	}
}
