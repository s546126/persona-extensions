package dingtalk

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

	"github.com/nwverse/persona-extensions/extensions/dingtalk/internal/dingtalk/api"
	"github.com/nwverse/persona-extensions/extensions/dingtalk/internal/dingtalk/api/apitest"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	ownerID   = "staff-owner"
	ownerNick = "Sample Owner"
	code      = "123456"
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
}

func newRig(t *testing.T, secrets *memSecrets) *rig {
	t.Helper()
	f := apitest.New(t)
	if secrets == nil {
		secrets = &memSecrets{}
	}
	r := &rig{t: t, f: f, hub: &memHub{}, secrets: secrets}
	r.start()
	return r
}

func (r *rig) start() {
	r.bot = New(Options{Hub: r.hub, Agent: "Persona/test",
		NewClient: func(id, secret string) (*api.Client, error) {
			return api.New(api.Options{ClientID: id, ClientSecret: secret, BaseURL: r.f.URL(), HTTP: r.f.Client()})
		},
		Code: func() string { return code }}, r.secrets)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.t.Cleanup(cancel)
	go r.bot.Run(ctx)
}

func (r *rig) login(secret string) wire.ConnectorPrompt {
	r.t.Helper()
	p, err := r.bot.LoginStep(context.Background(), stepCreds, map[string]string{"client_id": apitest.ClientID, "client_secret": secret})
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

func dm(id, from, nick, text string, f *apitest.Fake) map[string]any {
	return map[string]any{"msgId": id, "conversationType": "1", "conversationId": "cid-" + from, "senderStaffId": from,
		"senderNick": nick, "robotCode": apitest.ClientID, "sessionWebhook": f.Webhook(id),
		"sessionWebhookExpiredTime": time.Now().Add(time.Hour).UnixMilli(), "msgtype": "text", "text": map[string]string{"content": text}}
}

// pair binds the fake app and pairs the owner.
func (r *rig) pair() {
	r.t.Helper()
	if p := r.login(apitest.ClientSecret); p.Kind != wire.ConnectorPromptKindDeviceCode || p.UserCode != code || p.Step != stepPair {
		r.t.Fatalf("after the credentials: %+v", p)
	}
	r.f.WaitConnected(r.t)
	r.f.PushMessage(r.t, "fp", dm("m-pair", ownerID, ownerNick, " "+code+" ", r.f))
	apitest.Eventually(r.t, "the pairing", func() bool { return len(r.f.Replies()) > 0 })
	if got := r.f.Replies(); got[len(got)-1] != msg("chat.paired") {
		r.t.Fatalf("pairing replied %q", got)
	}
}

func TestCredentialsPromptAndRefusal(t *testing.T) {
	r := newRig(t, nil)
	p, err := r.bot.LoginStep(context.Background(), "", nil)
	if err != nil || p.Step != stepCreds || len(p.Fields) != 2 || !p.Fields[1].Secret || p.Fields[0].Secret {
		t.Fatalf("the first step: %+v, %v", p, err)
	}
	if p := r.login("wrong"); p.Step != stepCreds || p.Error == "" {
		t.Fatalf("wrong credentials: %+v", p)
	}
	if p, _ := r.bot.LoginStep(context.Background(), stepCreds, map[string]string{"client_id": " "}); p.Error != msg("login.creds_missing") {
		t.Fatalf("missing credentials: %+v", p)
	}
	if s := r.bot.Status(context.Background()); s.State != wire.ConnectorStateNotSignedIn || len(s.Guide) == 0 {
		t.Fatalf("status before binding: %+v", s)
	}
	if r.secrets.data != nil {
		t.Fatal("refused credentials were saved")
	}
}

func TestPairForwardAndRefuseOthers(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	if p := r.login(apitest.ClientSecret); p.UserCode != code {
		t.Fatalf("no pairing code: %+v", p)
	}
	r.f.WaitConnected(t)
	if s := r.bot.Status(ctx); s.State != wire.ConnectorStateNotSignedIn {
		t.Fatalf("status before pairing: %+v", s)
	}
	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "r0", Text: "x"}); !errors.Is(err, errCannotSend) {
		t.Fatalf("send before pairing: %v", err)
	}
	r.f.PushMessage(t, "f0", dm("m0", ownerID, ownerNick, "hello before pairing", r.f))
	apitest.Eventually(t, "the not-paired reply", func() bool { return len(r.f.Replies()) == 1 })
	r.f.PushMessage(t, "f1", dm("m1", ownerID, ownerNick, "000000", r.f))
	apitest.Eventually(t, "the wrong-code reply", func() bool { return len(r.f.Replies()) == 2 })
	r.f.PushMessage(t, "f2", dm("m2", ownerID, ownerNick, code, r.f))
	apitest.Eventually(t, "the paired reply", func() bool { return len(r.f.Replies()) == 3 })
	if got := r.f.Replies(); got[0] != msg("chat.not_paired") || got[1] != msg("chat.wrong_code") || got[2] != msg("chat.paired") {
		t.Fatalf("replies %q", got)
	}
	if p, err := r.bot.LoginStep(ctx, stepPair, nil); err != nil || !p.Done || p.Account != ownerNick {
		t.Fatalf("the pairing step after pairing: %+v, %v", p, err)
	}
	if s := r.bot.Status(ctx); s.State != wire.ConnectorStateOk || s.Account != ownerNick {
		t.Fatalf("status once paired: %+v", s)
	}
	if len(r.hub.messages()) != 0 {
		t.Fatalf("pairing messages reached the hub: %+v", r.hub.messages())
	}

	r.f.PushMessage(t, "f3", dm("m3", ownerID, ownerNick, "  what's on today?  ", r.f))
	r.f.PushMessage(t, "f3", dm("m3", ownerID, ownerNick, "  what's on today?  ", r.f)) // delivered again
	stranger := dm("m4", "staff-other", "Someone Else", "let me in", r.f)
	r.f.PushMessage(t, "f4", stranger)
	group := dm("m5", ownerID, ownerNick, "@robot in a group", r.f)
	group["conversationType"] = "2"
	r.f.PushMessage(t, "f5", group)
	r.f.PushMessage(t, "f6", dm("m6", ownerID, ownerNick, "second", r.f))
	apitest.Eventually(t, "the owner's messages", func() bool { return len(r.hub.messages()) == 2 })
	if got := r.hub.messages(); got[0].ID != "m3" || got[0].Text != "what's on today?" || got[1].ID != "m6" {
		t.Fatalf("the hub got %+v", got)
	}
	if got := r.f.Replies(); len(got) != 4 || got[3] != msg("chat.stranger") {
		t.Fatalf("the stranger heard %q", got)
	}
	for _, id := range []string{"f3", "f4", "f5", "f6"} {
		if r.f.AckOf(id) == nil {
			t.Errorf("frame %s was not answered", id)
		}
	}
	// the stranger hears it once in ten minutes
	r.f.PushMessage(t, "f7", dm("m7", "staff-other", "Someone Else", "again", r.f))
	r.f.PushMessage(t, "f8", dm("m8", ownerID, ownerNick, "third", r.f))
	apitest.Eventually(t, "the third message", func() bool { return len(r.hub.messages()) == 3 })
	if len(r.f.Replies()) != 4 {
		t.Fatalf("the stranger heard it twice: %q", r.f.Replies())
	}

	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "r1", Text: "# Today\n\nNothing **urgent**.", Kind: wire.ChannelSendKindAnswer}); err != nil {
		t.Fatal(err)
	}
	if s := r.f.Sent(); len(s) != 1 || s[0].StaffID != ownerID || s[0].Title != "Today" || !strings.Contains(s[0].Text, "**urgent**") {
		t.Fatalf("sent %+v", s)
	}
}

func TestPairingSurvivesRestartAndNewCredentialsPairAgain(t *testing.T) {
	r := newRig(t, nil)
	r.pair()
	r.cancel()
	r.start() // the same sealed state
	r.f.WaitConnected(t)
	apitest.Eventually(t, "the status after a restart", func() bool { return r.bot.Status(context.Background()).State == wire.ConnectorStateOk })
	r.f.PushMessage(t, "f9", dm("m9", ownerID, ownerNick, "after restart", r.f))
	r.f.PushMessage(t, "fp", dm("m-pair", ownerID, ownerNick, code, r.f)) // seen before the restart
	apitest.Eventually(t, "the message after a restart", func() bool { return len(r.hub.messages()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := r.hub.messages(); len(got) != 1 || got[0].Text != "after restart" {
		t.Fatalf("the hub got %+v", got)
	}
	if p := r.login(apitest.ClientSecret); p.Kind != wire.ConnectorPromptKindDeviceCode {
		t.Fatalf("binding again: %+v", p)
	}
	if s := r.bot.Status(context.Background()); s.State != wire.ConnectorStateNotSignedIn {
		t.Fatalf("new credentials kept the old owner: %+v", s)
	}
}

func TestPairingCodeIsVoidedAfterTooManyWrongTries(t *testing.T) {
	r := newRig(t, nil)
	r.login(apitest.ClientSecret)
	r.f.WaitConnected(t)
	for i := range pairTries {
		r.f.PushMessage(t, "w", dm("w"+string(rune('a'+i)), "staff-guess", "Guesser", "65432"+string(rune('0'+i)), r.f))
	}
	apitest.Eventually(t, "the wrong-code replies", func() bool { return len(r.f.Replies()) == pairTries })
	if got := r.f.Replies(); got[pairTries-1] != msg("chat.code_void") {
		t.Fatalf("replies %q", got)
	}
	if _, err := r.bot.LoginStep(context.Background(), stepPair, nil); err == nil {
		t.Fatal("a voided pairing still waits")
	}
	r.f.PushMessage(t, "late", dm("late", ownerID, ownerNick, code, r.f))
	apitest.Eventually(t, "the late reply", func() bool { return len(r.f.Replies()) == pairTries+1 })
	if s := r.bot.Status(context.Background()); s.State == wire.ConnectorStateOk {
		t.Fatal("the voided code still paired")
	}
}

func TestPairingCodeExpires(t *testing.T) {
	r := newRig(t, nil)
	now := time.Now()
	r.bot.opt.Now = func() time.Time { return now }
	r.login(apitest.ClientSecret)
	now = now.Add(pairFor + time.Second)
	if _, err := r.bot.LoginStep(context.Background(), stepPair, nil); err == nil || err.Error() != msg("login.pair_expired") {
		t.Fatalf("an expired pairing: %v", err)
	}
}

func TestMediaIsHandedOver(t *testing.T) {
	r := newRig(t, nil)
	r.pair()
	r.f.Files["pic"] = []byte("\x89PNG\r\n\x1a\nsynthetic")
	r.f.Files["big"] = make([]byte, imageMost+1)
	r.f.Files["doc"] = []byte("%PDF-1.4 synthetic")
	pic := dm("p1", ownerID, ownerNick, "", r.f)
	pic["msgtype"], pic["content"] = "picture", map[string]string{"downloadCode": "pic"}
	big := dm("p2", ownerID, ownerNick, "", r.f)
	big["msgtype"], big["content"] = "picture", map[string]string{"downloadCode": "big"}
	file := dm("p3", ownerID, ownerNick, "", r.f)
	file["msgtype"], file["content"] = "file", map[string]string{"downloadCode": "doc", "fileName": "notes.pdf"}
	voice := dm("p4", ownerID, ownerNick, "", r.f)
	voice["msgtype"], voice["content"] = "audio", map[string]string{"recognition": "remind me at five", "downloadCode": "v"}
	video := dm("p5", ownerID, ownerNick, "", r.f)
	video["msgtype"], video["content"] = "video", map[string]string{"downloadCode": "vid"}
	rich := dm("p6", ownerID, ownerNick, "", r.f)
	rich["msgtype"] = "richText"
	rich["content"] = map[string]any{"richText": []map[string]string{{"text": "see "}, {"type": "picture", "downloadCode": "pic"}, {"text": "this"}}}
	for i, m := range []map[string]any{pic, big, file, voice, video, rich} {
		r.f.PushMessage(t, "media"+string(rune('0'+i)), m)
	}
	apitest.Eventually(t, "the media messages", func() bool { return len(r.hub.messages()) == 6 })
	got := map[string]wire.ChannelMessage{}
	for _, m := range r.hub.messages() {
		got[m.ID] = m
	}
	if m := got["p1"]; len(m.Images) != 1 || m.Images[0].Mime != "image/png" {
		t.Errorf("a picture: %+v", m)
	}
	if m := got["p2"]; len(m.Images) != 0 || len(m.Unsupported) != 1 {
		t.Errorf("an oversized picture: %+v", m)
	}
	if m := got["p3"]; len(m.Files) != 1 || m.Files[0].Name != "notes.pdf" {
		t.Errorf("a file: %+v", m)
	} else if data, _ := base64.StdEncoding.DecodeString(m.Files[0].Data); string(data) != "%PDF-1.4 synthetic" {
		t.Errorf("the file's bytes: %q", data)
	}
	if m := got["p4"]; m.Text != "remind me at five" {
		t.Errorf("a voice message: %+v", m)
	}
	if m := got["p5"]; len(m.Unsupported) != 1 {
		t.Errorf("a video: %+v", m)
	}
	if m := got["p6"]; m.Text != "see this" || len(m.Images) != 1 {
		t.Errorf("rich text: %+v", m)
	}
}

// A message the hub fails to take is neither marked seen nor answered on the Stream, so DingTalk's redelivery reaches
// the hub once it is back; the owner is told once (review of #311).
func TestHubFailureLeavesTheMessageForRedelivery(t *testing.T) {
	r := newRig(t, nil)
	r.pair()
	r.hub.mu.Lock()
	r.hub.fail = true
	r.hub.mu.Unlock()
	n := len(r.f.Replies())
	r.f.PushMessage(t, "hf1", dm("hf", ownerID, ownerNick, "are you there", r.f))
	apitest.Eventually(t, "the hub error reply", func() bool { return len(r.f.Replies()) == n+1 })
	if got := r.f.Replies(); got[n] != msg("chat.hub_error") {
		t.Fatalf("replied %q", got[n])
	}
	r.f.PushMessage(t, "hf2", dm("hf", ownerID, ownerNick, "are you there", r.f)) // DingTalk delivers it again
	time.Sleep(100 * time.Millisecond)
	if r.f.AckOf("hf1") != nil || r.f.AckOf("hf2") != nil || len(r.f.Replies()) != n+1 {
		t.Fatalf("a message the hub did not take was answered, or the owner told twice: %q", r.f.Replies())
	}
	r.hub.mu.Lock()
	r.hub.fail = false
	r.hub.mu.Unlock()
	r.f.PushMessage(t, "hf3", dm("hf", ownerID, ownerNick, "are you there", r.f))
	apitest.Eventually(t, "the message at the hub", func() bool { return len(r.hub.messages()) == 1 })
	apitest.Eventually(t, "the answer", func() bool { return r.f.AckOf("hf3") != nil })
	r.f.PushMessage(t, "hf4", dm("hf", ownerID, ownerNick, "are you there", r.f)) // and a late copy is a duplicate
	apitest.Eventually(t, "the duplicate's answer", func() bool { return r.f.AckOf("hf4") != nil })
	if len(r.hub.messages()) != 1 {
		t.Fatal("the message reached the hub twice")
	}
}

// A stored state that does not read is reported and never overwritten by accident; binding again replaces it.
func TestUnreadableStateIsKept(t *testing.T) {
	secrets := &memSecrets{data: []byte("{not json")}
	r := newRig(t, secrets)
	apitest.Eventually(t, "the status", func() bool { return r.bot.Status(context.Background()).State == wire.ConnectorStateError })
	if string(secrets.data) != "{not json" {
		t.Fatal("the stored state was overwritten")
	}
	r.pair()
	if !strings.Contains(string(secrets.data), apitest.ClientID) {
		t.Fatal("binding again did not replace it")
	}
}

func TestSendErrors(t *testing.T) {
	sendRetryWait = time.Millisecond
	defer func() { sendRetryWait = time.Second }()
	r := newRig(t, nil)
	r.pair()
	ctx := context.Background()
	r.f.Invalid[ownerID] = true
	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "a", Text: "x"}); !errors.Is(err, errCannotSend) {
		t.Errorf("an unreachable owner: %v", err)
	}
	delete(r.f.Invalid, ownerID)
	r.f.Fail = 503
	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "b", Text: "x"}); err == nil || errors.Is(err, errCannotSend) {
		t.Errorf("a busy DingTalk: %v", err)
	}
	r.f.Fail = 0
	long := strings.Repeat("line of text\n", MaxText/10)
	if _, err := r.bot.Send(ctx, wire.ChannelSendParams{Ref: "c", Text: long}); err != nil {
		t.Fatal(err)
	}
	if s := r.f.Sent(); len(s) < 2 {
		t.Errorf("a long message went as %d pieces", len(s))
	}
}

func TestLogoutForgetsEverything(t *testing.T) {
	r := newRig(t, nil)
	r.pair()
	if err := r.bot.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.secrets.data != nil {
		t.Error("the sealed state was kept")
	}
	if s := r.bot.Status(context.Background()); s.State != wire.ConnectorStateNotSignedIn {
		t.Errorf("status after logout: %+v", s)
	}
	if _, err := r.bot.Send(context.Background(), wire.ChannelSendParams{Ref: "x", Text: "x"}); !errors.Is(err, errCannotSend) {
		t.Errorf("send after logout: %v", err)
	}
}

// Chunk keeps every character but newlines at a cut, in order, and never gives an empty piece, even for long runs of
// blank lines around the cut (review of #311).
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

func TestChunkAndTitle(t *testing.T) {
	if got := Chunk("", 10); got != nil {
		t.Errorf("empty: %q", got)
	}
	got := Chunk("aaaa\nbbbbbb\ncc", 8)
	if strings.Join(got, "|") != "aaaa|bbbbbb|cc" {
		t.Errorf("chunks %q", got)
	}
	for _, p := range Chunk(strings.Repeat("字", 25), 10) {
		if n := len([]rune(p)); n > 10 {
			t.Errorf("a piece of %d runes", n)
		}
	}
	cases := map[string]string{
		"# Weekly report\nbody": "Weekly report",
		"\n\n> **Note**: hi":    "Note: hi",
		"```go\ncode\n```":      "code",
		"":                      "Persona",
		strings.Repeat("长", 40): strings.Repeat("长", titleMost-1) + "…",
	}
	for in, want := range cases {
		if got := Title(in); got != want {
			t.Errorf("Title(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFileMimeFallsBackToTheExtension(t *testing.T) {
	if got := fileMime("notes.pdf", []byte("%PDF-1.4")); got != "application/pdf" {
		t.Errorf("from the bytes: %s", got)
	}
	if got := fileMime("form.doc", []byte{0xd0, 0xcf, 0x11, 0xe0, 0, 1, 2}); got == "application/octet-stream" || got == "" {
		t.Errorf("an old Word file by its extension: %s", got)
	}
	if got := fileMime("blob", []byte{0, 1, 2}); got != "application/octet-stream" {
		t.Errorf("nothing to go by: %s", got)
	}
}

func TestLocalesHaveTheSameKeys(t *testing.T) {
	want := slices.Sorted(maps.Keys(catalogs[langEn]))
	for _, l := range []string{langZhHans, langJa} {
		if got := slices.Sorted(maps.Keys(catalogs[l])); !slices.Equal(got, want) {
			t.Errorf("%s has keys %v, en has %v", l, got, want)
		}
	}
	if langOf("") != langZhHans || langOf("fr") != langEn || langOf(langJa) != langJa {
		t.Error("the language fallback changed")
	}
}

// msg is a text of the default language (the tests' robot is not told one).
func msg(key string) string { return t("", key) }
