package wechat

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wechat/internal/wechat/ilink"
	"github.com/nwverse/persona/sdk/go/wire"
)

func testQuotePolicy() quotePolicy {
	return quotePolicy{textTTL: 30 * 24 * time.Hour, mediaTTL: 7 * 24 * time.Hour, maxEntries: 32, maxBytes: 4096}
}

func TestQuoteStorePersistsAndExpires(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	policy := testQuotePolicy()
	s := openQuoteStoreWithPolicy(dir, clock, policy)
	s.put("text", "the original words", nil)
	s.put("media", "", []quoteMedia{{Kind: "image"}, {Kind: "file", Name: "report.pdf", Size: 123}})

	info, err := os.Stat(filepath.Join(dir, quoteFileName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("quote index mode = %04o, want 0600", info.Mode().Perm())
	}

	s = openQuoteStoreWithPolicy(dir, clock, policy)
	if got := s.lookup("text"); got != "the original words" {
		t.Fatalf("text after restart = %q", got)
	}
	if got := s.lookup("media"); !strings.Contains(got, "[图片]") || !strings.Contains(got, "report.pdf") || !strings.Contains(got, "123") {
		t.Fatalf("media after restart = %q", got)
	}

	now = now.Add(8 * 24 * time.Hour)
	s = openQuoteStoreWithPolicy(dir, clock, policy)
	if got := s.lookup("media"); got != "" {
		t.Fatalf("media survived its seven-day retention: %q", got)
	}
	if got := s.lookup("text"); got != "the original words" {
		t.Fatalf("text should still be retained: %q", got)
	}

	now = now.Add(23 * 24 * time.Hour)
	s = openQuoteStoreWithPolicy(dir, clock, policy)
	if got := s.lookup("text"); got != "" {
		t.Fatalf("text survived its thirty-day retention: %q", got)
	}
}

func TestQuoteStoreMixedEntryKeepsTextAfterMediaExpires(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	policy := testQuotePolicy()
	s := openQuoteStoreWithPolicy(dir, func() time.Time { return now }, policy)
	s.put("mixed", "caption", []quoteMedia{{Kind: "video", Name: "clip.mp4", Size: 42}})
	if got := s.lookup("mixed"); !strings.Contains(got, "caption") || !strings.Contains(got, "clip.mp4") {
		t.Fatalf("mixed quote = %q", got)
	}
	now = now.Add(8 * 24 * time.Hour)
	if got := s.lookup("mixed"); got != "caption" {
		t.Fatalf("mixed quote after media expiry = %q", got)
	}
}

func TestQuoteStoreEvictsByCountAndBytes(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	policy := quotePolicy{textTTL: time.Hour, mediaTTL: time.Hour, maxEntries: 2, maxBytes: 1024}
	dir := t.TempDir()
	s := openQuoteStoreWithPolicy(dir, clock, policy)
	s.put("one", "first", nil)
	now = now.Add(time.Second)
	s.put("two", "second", nil)
	now = now.Add(time.Second)
	s.put("three", "third", nil)
	if got := s.lookup("one"); got != "" {
		t.Fatalf("oldest entry survived the count limit: %q", got)
	}
	if got := s.lookup("three"); got != "third" {
		t.Fatalf("newest entry = %q", got)
	}
	s = openQuoteStoreWithPolicy(dir, clock, policy)
	if got := s.lookup("one"); got != "" {
		t.Fatalf("evicted entry returned after restart: %q", got)
	}

	policy = quotePolicy{textTTL: time.Hour, mediaTTL: time.Hour, maxEntries: 32, maxBytes: 80}
	dir = t.TempDir()
	s = openQuoteStoreWithPolicy(dir, clock, policy)
	for i, id := range []string{"a", "b", "c", "d"} {
		now = now.Add(time.Second)
		s.put(id, strings.Repeat(fmt.Sprintf("%d", i), 24), nil)
	}
	if s.bytes > policy.maxBytes {
		t.Fatalf("stored %d content bytes, limit %d", s.bytes, policy.maxBytes)
	}
	if got := s.lookup("a"); got != "" {
		t.Fatalf("oldest entry survived the byte limit: %q", got)
	}
	if got := s.lookup("d"); got == "" {
		t.Fatal("newest entry was not retained")
	}
	s = openQuoteStoreWithPolicy(dir, clock, policy)
	if got := s.lookup("a"); got != "" {
		t.Fatalf("byte-evicted entry returned after restart: %q", got)
	}
}

func TestQuoteStoreCorruptFileStartsEmptyAndRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, quoteFileName)
	if err := os.WriteFile(path, []byte("{not valid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := testQuotePolicy()
	s := openQuoteStoreWithPolicy(dir, time.Now, policy)
	if got := s.lookup("anything"); got != "" {
		t.Fatalf("corrupt cache returned %q", got)
	}
	s.put("new", "usable", nil)
	s = openQuoteStoreWithPolicy(dir, time.Now, policy)
	if got := s.lookup("new"); got != "usable" {
		t.Fatalf("cache did not recover after corruption: %q", got)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".quote-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary quote files left behind: %v, %v", matches, err)
	}
}

func TestQuoteStoreConcurrentUse(t *testing.T) {
	policy := testQuotePolicy()
	policy.maxEntries = 8
	s := openQuoteStoreWithPolicy(t.TempDir(), time.Now, policy)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				id := fmt.Sprintf("%d-%d", worker, i)
				s.put(id, "body", []quoteMedia{{Kind: "image"}})
				_ = s.lookup(id)
			}
		}()
	}
	wg.Wait()
	if len(s.entries) != policy.maxEntries || s.bytes > policy.maxBytes {
		t.Fatalf("concurrent store has %d entries and %d bytes", len(s.entries), s.bytes)
	}
}

func queueQuoteMessage(r *rig, id int64, items ...map[string]any) {
	r.ilink.mu.Lock()
	defer r.ilink.mu.Unlock()
	r.ilink.queue = append(r.ilink.queue, map[string]any{
		"message_id": id, "from_user_id": owner, "context_token": fmt.Sprintf("quote-context-%d", id),
		"message_type": 1, "item_list": items,
	})
}

func TestRestoresTextQuoteBySvrID(t *testing.T) {
	r := newRig(t, true)
	queueQuoteMessage(r, 9101, map[string]any{"type": 1, "text_item": map[string]any{"text": "被引用的原文"}})
	eventually(t, "the original at the hub", func() bool { return len(r.hub.got()) == 1 })
	queueQuoteMessage(r, 9102, map[string]any{"type": 1, "text_item": map[string]any{"text": "这是回复"},
		"ref_msg": map[string]any{"title": "原消息", "svr_id": "9101"}})
	eventually(t, "the restored quote at the hub", func() bool { return len(r.hub.got()) == 2 })
	got := r.hub.got()[1]
	if got.Text != "这是回复" || got.Quote != "原消息\n被引用的原文" {
		t.Fatalf("restored message = %+v", got)
	}
}

func TestRestoresImageQuoteBySvrID(t *testing.T) {
	r := newRig(t, true)
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	enc, err := ilink.EncryptECB(png, key)
	if err != nil {
		t.Fatal(err)
	}
	r.ilink.mu.Lock()
	r.ilink.downloads = map[string][]byte{"quote-image": enc}
	r.ilink.mu.Unlock()
	queueQuoteMessage(r, 9201, map[string]any{"type": 2, "image_item": map[string]any{
		"aeskey": hex.EncodeToString(key), "media": map[string]any{"encrypt_query_param": "quote-image", "aes_key": base64.StdEncoding.EncodeToString(key)}}})
	eventually(t, "the original image at the hub", func() bool { return len(r.hub.got()) == 1 })
	queueQuoteMessage(r, 9202, map[string]any{"type": 1, "text_item": map[string]any{"text": "看这张"},
		"ref_msg": map[string]any{"svr_id": "9201"}})
	eventually(t, "the restored image quote at the hub", func() bool { return len(r.hub.got()) == 2 })
	got := r.hub.got()[1]
	if got.Text != "看这张" || got.Quote != "[图片]" {
		t.Fatalf("restored image quote = %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(r.bot.opt.DataDir, quoteFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"quote-image", hex.EncodeToString(key), base64.StdEncoding.EncodeToString(key), base64.StdEncoding.EncodeToString(png), "https://"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("quote index contains media transport data %q", forbidden)
		}
	}
}

func TestUnknownSvrIDKeepsSafeFallback(t *testing.T) {
	r := newRig(t, true)
	queueQuoteMessage(r, 9301, map[string]any{"type": 1, "text_item": map[string]any{"text": "主消息不能丢"},
		"ref_msg": map[string]any{"title": "只留下了标题", "svr_id": "not-in-the-cache"}})
	eventually(t, "the fallback quote at the hub", func() bool { return len(r.hub.got()) == 1 })
	got := r.hub.got()[0]
	if got.Text != "主消息不能丢" || got.Quote != "只留下了标题" {
		t.Fatalf("fallback message = %+v", got)
	}
}

func TestPartialTextRestoresSelectedRange(t *testing.T) {
	r := newRig(t, true)
	queueQuoteMessage(r, 9351, map[string]any{"type": 1, "text_item": map[string]any{"text": "A B C A B C"}})
	eventually(t, "the long original at the hub", func() bool { return len(r.hub.got()) == 1 })
	queueQuoteMessage(r, 9352, map[string]any{"type": 1, "text_item": map[string]any{"text": "selected"},
		"ref_msg": map[string]any{"svr_id": "9351", "partial_text": map[string]any{"start": "B", "end": "C", "startindex": 1, "endindex": 1}}})
	eventually(t, "the selected quote at the hub", func() bool { return len(r.hub.got()) == 2 })
	if got := r.hub.got()[1].Quote; got != "B C" {
		t.Fatalf("selected quote = %q", got)
	}
	queueQuoteMessage(r, 9353, map[string]any{"type": 1, "text_item": map[string]any{"text": "bad hash"},
		"ref_msg": map[string]any{"svr_id": "9351", "partial_text": map[string]any{"start": "B", "end": "C", "startindex": 0, "endindex": 1, "quotemd5": "incorrect"}}})
	eventually(t, "the rejected partial quote at the hub", func() bool { return len(r.hub.got()) == 3 })
	if got := r.hub.got()[2]; got.Quote != "" || got.Text != "bad hash" {
		t.Fatalf("invalid partial quote leaked full text: %+v", got)
	}
}

func TestQuoteCacheIsScopedAndReset(t *testing.T) {
	b := New(Options{DataDir: t.TempDir()}, nil)
	b.compose(t.Context(), nil, "account-a", ilink.Message{SvrID: "42", Text: "private text"})
	data, err := os.ReadFile(b.quotes.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "account-a") {
		t.Fatal("quote index stores a plaintext account identifier")
	}
	if got := b.compose(t.Context(), nil, "account-b", ilink.Message{QuoteID: "42", Text: "reply"}); got.Quote != "" {
		t.Fatalf("another account restored %q", got.Quote)
	}
	if got := b.compose(t.Context(), nil, "account-a", ilink.Message{QuoteID: "42", Text: "reply"}); got.Quote != "private text" {
		t.Fatalf("same account quote = %q", got.Quote)
	}
	b.quotes.reset()
	if got := b.compose(t.Context(), nil, "account-a", ilink.Message{QuoteID: "42", Text: "reply"}); got.Quote != "" {
		t.Fatalf("reset cache restored %q", got.Quote)
	}
}

func TestLogoutClearsQuoteCache(t *testing.T) {
	r := newRig(t, true)
	queueQuoteMessage(r, 9361, map[string]any{"type": 1, "text_item": map[string]any{"text": "before logout"}})
	eventually(t, "the cached message at the hub", func() bool { return len(r.hub.got()) == 1 })
	if got := r.bot.quotes.lookup(quoteCacheID(owner, "9361")); got != "before logout" {
		t.Fatalf("before logout = %q", got)
	}
	if err := r.bot.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.bot.quotes.lookup(quoteCacheID(owner, "9361")); got != "" {
		t.Fatalf("logout retained %q", got)
	}
}

func TestAccountChangeClearsQuoteCache(t *testing.T) {
	secrets := &memSecrets{}
	b := New(Options{DataDir: t.TempDir()}, secrets)
	b.st.Creds = &ilink.Credentials{BotID: "old-bot", UserID: "account-a", Token: "old-token"}
	b.loaded = true
	b.quotes.put(quoteCacheID("account-a", "42"), "private text", nil)
	b.bound(t.Context(), ilink.Credentials{BotID: "new-bot", UserID: "account-b", Token: "new-token"})
	if got := b.quotes.lookup(quoteCacheID("account-a", "42")); got != "" {
		t.Fatalf("account change retained %q", got)
	}
}

func TestOutboundTextIsAvailableForQuoteRestore(t *testing.T) {
	r := newRig(t, true)
	r.ilink.say(owner, "先建立会话")
	eventually(t, "the first message at the hub", func() bool { return len(r.hub.got()) == 1 })
	r.ilink.mu.Lock()
	r.ilink.sendID = "outbound-server-id"
	r.ilink.mu.Unlock()
	if _, err := r.bot.Send(t.Context(), wire.ChannelSendParams{Ref: "outbound-ref", Text: "机器人发出的原文"}); err != nil {
		t.Fatal(err)
	}
	r.ilink.mu.Lock()
	r.ilink.queue = append(r.ilink.queue, map[string]any{
		"message_id": 9402, "from_user_id": owner, "context_token": "quote-context-outbound", "message_type": 1,
		"item_list": []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "引用回答"},
			"ref_msg": map[string]any{"svr_id": "outbound-server-id"}}},
	})
	r.ilink.mu.Unlock()
	eventually(t, "the outbound quote at the hub", func() bool { return len(r.hub.got()) == 2 })
	if got := r.hub.got()[1].Quote; got != "机器人发出的原文" {
		t.Fatalf("outbound quote = %q", got)
	}
}
