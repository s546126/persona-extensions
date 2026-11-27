package wecom

import (
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wecom/internal/wecom/api"
	"github.com/nwverse/persona-extensions/extensions/wecom/internal/wecom/api/apitest"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	ownerID = "user-owner"
	code    = "123456"
)

type memSecrets struct {
	mu   sync.Mutex
	data []byte
}

func (s *memSecrets) Load(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.data), nil
}
func (s *memSecrets) Save(_ context.Context, d []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = slices.Clone(d)
	return nil
}
func (s *memSecrets) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = nil
	return nil
}

type memHub struct {
	mu   sync.Mutex
	got  []wire.ChannelMessage
	fail bool
}

func (h *memHub) Receive(_ context.Context, m wire.ChannelMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail {
		return errors.New("the hub is away")
	}
	h.got = append(h.got, m)
	return nil
}

func (h *memHub) messages() []wire.ChannelMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.got)
}

type rig struct {
	t       *testing.T
	f       *apitest.Fake
	hub     *memHub
	secrets *memSecrets
	bot     *Bot
	cancel  context.CancelFunc
	kicked  time.Duration
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, f: apitest.New(t), hub: &memHub{}, secrets: &memSecrets{}, kicked: time.Hour}
	r.start()
	return r
}

func (r *rig) start() {
	r.bot = New(Options{Hub: r.hub, KickedWait: r.kicked,
		NewClient: func(id, secret string) (*api.Client, error) { return api.New(r.f.Options(id, secret)) },
		Code:      func() string { return code }}, r.secrets)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.t.Cleanup(cancel)
	go r.bot.Run(ctx)
}

func (r *rig) login(secret string) wire.ConnectorPrompt {
	r.t.Helper()
	p, err := r.bot.LoginStep(context.Background(), stepCreds, map[string]string{"bot_id": apitest.BotID, "secret": secret})
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

// sentTo are the texts sent to userID.
func (r *rig) sentTo(userID string) []string {
	var out []string
	for _, s := range r.f.Sent() {
		if s.UserID == userID {
			out = append(out, s.Text)
		}
	}
	return out
}

// pair binds the fake robot and pairs the owner.
func (r *rig) pair() {
	r.t.Helper()
	if p := r.login(apitest.Secret); p.Kind != wire.ConnectorPromptKindDeviceCode || p.UserCode != code {
		r.t.Fatalf("after the credentials: %+v", p)
	}
	r.f.WaitConnected(r.t) // the check
	r.f.WaitConnected(r.t) // the connection it keeps
	apitest.Eventually(r.t, "the session", func() bool {
		r.bot.mu.Lock()
		defer r.bot.mu.Unlock()
		return r.bot.session != nil
	})
	r.f.Text(r.t, "m-pair", ownerID, " "+code+" ")
	apitest.Eventually(r.t, "the pairing", func() bool { return len(r.sentTo(ownerID)) > 0 })
	if got := r.sentTo(ownerID); got[len(got)-1] != msg("chat.paired") {
		r.t.Fatalf("pairing answered %q", got)
	}
}

func TestCredentialsPromptAndRefusal(t *testing.T) {
	r := newRig(t)
	p, err := r.bot.LoginStep(context.Background(), "", nil)
	if err != nil || p.Step != stepCreds || len(p.Fields) != 2 || !p.Fields[1].Secret {
		t.Fatalf("the first step: %+v, %v", p, err)
	}
	if p := r.login("wrong"); p.Step != stepCreds || p.Error == "" {
		t.Fatalf("wrong credentials: %+v", p)
	}
	if s := r.bot.Status(context.Background()); s.State != wire.ConnectorStateNotSignedIn || len(s.Guide) == 0 {
		t.Fatalf("status before binding: %+v", s)
	}
	if r.secrets.data != nil {
		t.Fatal("refused credentials were saved")
	}
}

func TestPairForwardAndRefuseOthers(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.pair()
	if p, err := r.bot.LoginStep(ctx, stepPair, nil); err != nil || !p.Done {
		t.Fatalf("the pairing step after pairing: %+v, %v", p, err)
	}
	if s := r.bot.Status(ctx); s.State != wire.ConnectorStateOk {
		t.Fatalf("status once paired: %+v", s)
	}
	r.f.Text(t, "m1", ownerID, "  what's on today?  ")
	r.f.Text(t, "m1", ownerID, "  what's on today?  ") // delivered again
	r.f.Text(t, "m2", "user-other", "let me in")
	r.f.Message(t, "m3", ownerID, map[string]any{"chattype": "group", "chatid": "group-1", "msgtype": "text", "text": map[string]string{"content": "@robot hi"}})
	r.f.Message(t, "m4", ownerID, map[string]any{"msgtype": "text", "text": map[string]string{"content": "about that"},
		"quote": map[string]any{"msgtype": "text", "text": map[string]string{"content": "an earlier line"}}})
	apitest.Eventually(t, "the owner's messages", func() bool { return len(r.hub.messages()) == 2 })
	got := r.hub.messages()
	if got[0].ID != "m1" || got[0].Text != "what's on today?" || got[1].Quote != "an earlier line" {
		t.Fatalf("the hub got %+v", got)
	}
	apitest.Eventually(t, "the stranger's answer", func() bool { return len(r.sentTo("user-other")) == 1 })
	if s := r.sentTo("user-other"); s[0] != msg("chat.stranger") {
		t.Fatalf("the stranger heard %q", s)
	}
	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "r1", Text: "# Today\n\nNothing **urgent**."}); err != nil {
		t.Fatal(err)
	}
	if s := r.sentTo(ownerID); !strings.Contains(s[len(s)-1], "**urgent**") {
		t.Fatalf("sent %q", s)
	}
}

func TestBeforePairing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "r0", Text: "x"}); !errors.Is(err, errCannotSend) {
		t.Fatalf("send before binding: %v", err)
	}
	r.login(apitest.Secret)
	r.f.WaitConnected(t)
	r.f.WaitConnected(t)
	apitest.Eventually(t, "the session", func() bool { r.bot.mu.Lock(); defer r.bot.mu.Unlock(); return r.bot.session != nil })
	r.f.Text(t, "a", "user-guess", "hello")
	apitest.Eventually(t, "the not-paired answer", func() bool { return len(r.sentTo("user-guess")) == 1 })
	for i := range pairTries {
		r.f.Text(t, "w"+string(rune('a'+i)), "user-guess", "65432"+string(rune('0'+i)))
	}
	apitest.Eventually(t, "the wrong-code answers", func() bool { return len(r.sentTo("user-guess")) == pairTries+1 })
	if s := r.sentTo("user-guess"); s[0] != msg("chat.not_paired") || s[1] != msg("chat.wrong_code") || s[pairTries] != msg("chat.code_void") {
		t.Fatalf("answers %q", s)
	}
	if _, err := r.bot.LoginStep(ctx, stepPair, nil); err == nil {
		t.Fatal("a voided pairing still waits")
	}
	if len(r.hub.messages()) != 0 {
		t.Fatalf("a stranger reached the hub: %+v", r.hub.messages())
	}
}

func TestMediaIsDecryptedAndHandedOver(t *testing.T) {
	r := newRig(t)
	r.pair()
	picURL, picKey := r.f.Media("pic", []byte("\x89PNG\r\n\x1a\nsynthetic"))
	docURL, docKey := r.f.MediaNamed("doc", "form.doc", []byte{0xd0, 0xcf, 0x11, 0xe0, 1, 2, 3})
	bigURL, bigKey := r.f.Media("big", make([]byte, imageMost+1))
	r.f.Message(t, "p1", ownerID, map[string]any{"msgtype": "image", "image": map[string]string{"url": picURL, "aeskey": picKey}})
	r.f.Message(t, "p2", ownerID, map[string]any{"msgtype": "file", "file": map[string]string{"url": docURL, "aeskey": docKey}})
	r.f.Message(t, "p3", ownerID, map[string]any{"msgtype": "image", "image": map[string]string{"url": bigURL, "aeskey": bigKey}})
	r.f.Message(t, "p4", ownerID, map[string]any{"msgtype": "voice", "voice": map[string]string{"content": "remind me at five"}})
	r.f.Message(t, "p5", ownerID, map[string]any{"msgtype": "mixed", "mixed": map[string]any{"msg_item": []map[string]any{
		{"msgtype": "text", "text": map[string]string{"content": "see "}},
		{"msgtype": "image", "image": map[string]string{"url": picURL, "aeskey": picKey}},
		{"msgtype": "text", "text": map[string]string{"content": "this"}}}}})
	r.f.Message(t, "p6", ownerID, map[string]any{"msgtype": "video", "video": map[string]string{"url": picURL, "aeskey": picKey}})
	apitest.Eventually(t, "the media messages", func() bool { return len(r.hub.messages()) == 6 })
	got := map[string]wire.ChannelMessage{}
	for _, m := range r.hub.messages() {
		got[m.ID] = m
	}
	if m := got["p1"]; len(m.Images) != 1 || m.Images[0].Mime != "image/png" {
		t.Errorf("a picture: %+v", m)
	}
	if m := got["p2"]; len(m.Files) != 1 || m.Files[0].Name != "form.doc" || m.Files[0].Mime == "application/octet-stream" {
		t.Errorf("a file: %+v", m)
	} else if data, _ := base64.StdEncoding.DecodeString(m.Files[0].Data); len(data) != 7 {
		t.Errorf("the file's bytes: %q", data)
	}
	if m := got["p3"]; len(m.Images) != 0 || len(m.Unsupported) != 1 {
		t.Errorf("an oversized picture: %+v", m)
	}
	if m := got["p4"]; m.Text != "remind me at five" {
		t.Errorf("a voice message: %+v", m)
	}
	if m := got["p5"]; m.Text != "see this" || len(m.Images) != 1 {
		t.Errorf("a mixed message: %+v", m)
	}
	if m := got["p6"]; len(m.Unsupported) != 1 {
		t.Errorf("a video: %+v", m)
	}
}

func TestTakenOverWaitsInsteadOfFighting(t *testing.T) {
	r := newRig(t)
	r.pair()
	n := r.f.Subscribes()
	r.f.Kick(t)
	apitest.Eventually(t, "the takeover status", func() bool {
		s := r.bot.Status(context.Background())
		return s.State == wire.ConnectorStateError && s.Error == msg("wecom.kicked")
	})
	time.Sleep(100 * time.Millisecond)
	if r.f.Subscribes() != n {
		t.Fatal("it took the robot back at once")
	}
	if _, err := r.bot.Send(context.Background(), wire.ChannelSendParams{Ref: "k", Text: "x"}); !errors.Is(err, errCannotSend) {
		t.Fatalf("send while taken over: %v", err)
	}
}

func TestSendRefusedAndLogout(t *testing.T) {
	r := newRig(t)
	r.pair()
	ctx := context.Background()
	r.f.Set(func(x *apitest.Fake) { x.Refuse = 40096 })
	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "a", Text: "x"}); err == nil || errors.Is(err, errCannotSend) {
		t.Errorf("a refused send: %v", err)
	}
	r.f.Set(func(x *apitest.Fake) { x.Refuse = 0 })
	if err := r.bot.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if r.secrets.data != nil {
		t.Error("the sealed state was kept")
	}
	if s := r.bot.Status(ctx); s.State != wire.ConnectorStateNotSignedIn {
		t.Errorf("status after logout: %+v", s)
	}
}

func TestHubFailureIsToldInTheChat(t *testing.T) {
	r := newRig(t)
	r.pair()
	r.hub.mu.Lock()
	r.hub.fail = true
	r.hub.mu.Unlock()
	n := len(r.sentTo(ownerID))
	r.f.Text(t, "hf", ownerID, "are you there")
	apitest.Eventually(t, "the hub error answer", func() bool { return len(r.sentTo(ownerID)) == n+1 })
	if s := r.sentTo(ownerID); s[n] != msg("chat.hub_error") {
		t.Fatalf("answered %q", s[n])
	}
	// not counted as seen: a copy delivered again is tried again, the owner told only once (review of #314)
	r.f.Text(t, "hf", ownerID, "are you there")
	time.Sleep(100 * time.Millisecond)
	if len(r.sentTo(ownerID)) != n+1 {
		t.Fatalf("told twice: %q", r.sentTo(ownerID))
	}
	r.hub.mu.Lock()
	r.hub.fail = false
	r.hub.mu.Unlock()
	r.f.Text(t, "hf", ownerID, "are you there")
	apitest.Eventually(t, "the message at the hub", func() bool { return len(r.hub.messages()) == 1 })
	r.f.Text(t, "hf", ownerID, "are you there") // and once taken, a copy is a duplicate
	time.Sleep(100 * time.Millisecond)
	if len(r.hub.messages()) != 1 {
		t.Fatal("the message reached the hub twice")
	}
}

// Binding again with the robot already connected does not end with the robot's own connection taken over: the
// connection is ended before the check subscribes, then started again (review of #314).
func TestBindingAgainDoesNotKickItself(t *testing.T) {
	r := newRig(t)
	r.pair()
	if p := r.login(apitest.Secret); p.Kind != wire.ConnectorPromptKindDeviceCode {
		t.Fatalf("binding again: %+v", p)
	}
	apitest.Eventually(t, "the connection again", func() bool {
		r.bot.mu.Lock()
		defer r.bot.mu.Unlock()
		return r.bot.session != nil && r.bot.problem == ""
	})
	r.f.Text(t, "m-pair-2", ownerID, code)
	apitest.Eventually(t, "the pairing again", func() bool {
		return r.bot.Status(context.Background()).State == wire.ConnectorStateOk
	})
}

// A stored state that does not read is reported and never overwritten by accident; binding again replaces it.
func TestUnreadableStateIsKept(t *testing.T) {
	r := &rig{t: t, f: apitest.New(t), hub: &memHub{}, secrets: &memSecrets{data: []byte("{not json")}, kicked: time.Hour}
	r.start()
	apitest.Eventually(t, "the status", func() bool { return r.bot.Status(context.Background()).State == wire.ConnectorStateError })
	if string(r.secrets.data) != "{not json" {
		t.Fatal("the stored state was overwritten")
	}
	r.pair()
	if !strings.Contains(string(r.secrets.data), apitest.BotID) {
		t.Fatal("binding again did not replace it")
	}
}

// Chunk keeps every character but newlines at a cut, in order, and never gives an empty piece (review of #314).
func TestChunkKeepsTextAndSkipsEmptyPieces(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("\n", 25) + "tail",
		strings.Repeat("a", 7) + strings.Repeat("\n", 30) + "b" + strings.Repeat("\n", 9) + "c",
		strings.Repeat("ab\n\n\n", 20),
	} {
		pieces := Chunk(text, 10)
		for _, p := range pieces {
			if p == "" || len([]rune(p)) > 10 {
				t.Fatalf("Chunk(%q) gave piece %q", text, p)
			}
		}
		if got, want := strings.ReplaceAll(strings.Join(pieces, ""), "\n", ""), strings.ReplaceAll(text, "\n", ""); got != want {
			t.Fatalf("Chunk(%q) kept %q, want %q", text, got, want)
		}
	}
}

func TestChunk(t *testing.T) {
	if got := Chunk("aaaa\nbbbbbb\ncc", 8); strings.Join(got, "|") != "aaaa|bbbbbb|cc" {
		t.Errorf("chunks %q", got)
	}
	for _, p := range Chunk(strings.Repeat("字", 25), 10) {
		if n := len([]rune(p)); n > 10 {
			t.Errorf("a piece of %d runes", n)
		}
	}
}

func TestLocalesHaveTheSameKeys(t *testing.T) {
	want := slices.Sorted(maps.Keys(catalogs[langEn]))
	for _, l := range []string{langZhHans, langJa} {
		if got := slices.Sorted(maps.Keys(catalogs[l])); !slices.Equal(got, want) {
			t.Errorf("%s has keys %v, en has %v", l, got, want)
		}
	}
}

// msg is a text of the default language (the tests' robot is not told one).
func msg(key string) string { return t("", key) }

// The most a message may bring still encodes under the extension protocol's frame line; past it, blobs are dropped
// and named rather than the connection lost (the issue found in review of #311 applies here too).
func TestFullBlobBudgetFitsTheFrame(t *testing.T) {
	blob := func(n int) wire.ChannelBlob {
		return wire.ChannelBlob{Name: "f", Mime: "application/octet-stream", Data: strings.Repeat("A", (n+2)/3*4)}
	}
	m := wire.ChannelMessage{ID: "m", Text: strings.Repeat("字", 64<<10)}
	for range imagesMost {
		m.Images = append(m.Images, blob(imageMost))
	}
	m.Files = []wire.ChannelBlob{blob(blobsMost - imagesMost*imageMost)}
	if n := frameSize(m); n > frameMost {
		t.Fatalf("a full message encodes to %d bytes, over %d", n, frameMost)
	}
	m.Files = append(m.Files, blob(20<<20))
	fitFrame(&m)
	if frameSize(m) > frameMost || len(m.Files) != 1 || len(m.Unsupported) != 1 {
		t.Fatalf("after fitting: %d bytes, %d files, %q", frameSize(m), len(m.Files), m.Unsupported)
	}
}
