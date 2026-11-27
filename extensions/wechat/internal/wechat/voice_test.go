package wechat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wechat/internal/wechat/ilink"
	"github.com/nwverse/persona/sdk/go/wire"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func encryptVoiceFixture(t *testing.T, plain, key []byte) []byte {
	t.Helper()
	enc, err := ilink.EncryptECB(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// A voice message with WeChat's own transcript keeps the fast path: no CDN download, no file, no unsupported note.
func TestTranscribedVoiceSkipsAudioDownload(t *testing.T) {
	f := newFakeILink(t)
	hub := &fakeHub{}
	secrets := &memSecrets{}
	st := state{Creds: &ilink.Credentials{Token: "BT", BotID: "bot@im.bot", UserID: owner, BaseURL: "https://ilinkai.weixin.qq.com/"}}
	secrets.data, _ = json.Marshal(st)
	var downloads atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/c2c/download" {
			downloads.Add(1)
		}
		return f.RoundTrip(req)
	})
	bot := New(Options{
		Hub:     hub,
		Agent:   "Persona/test",
		Now:     func() time.Time { return time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC) },
		DataDir: t.TempDir(),
		NewClient: func(base, token string) (*ilink.Client, error) {
			return ilink.New(ilink.Options{BaseURL: base, Token: token, Agent: "Persona/test", HTTP: &http.Client{Transport: transport}})
		},
	}, secrets)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go bot.Run(ctx)

	f.say(owner, "", map[string]any{
		"type": 3,
		"voice_item": map[string]any{
			"text":        "明天上午十点开会",
			"encode_type": ilink.VoiceEncodeSILK,
			"media":       map[string]any{"encrypt_query_param": "should-not-download", "aes_key": "invalid-key"},
		},
	})
	eventually(t, "the transcribed voice message at the hub", func() bool { return len(hub.got()) == 1 })
	m := hub.got()[0]
	if m.Text != "明天上午十点开会" || len(m.Files) != 0 || len(m.Unsupported) != 0 {
		t.Fatalf("transcribed voice handed %+v", m)
	}
	if downloads.Load() != 0 {
		t.Fatalf("transcribed voice triggered %d CDN downloads, want 0", downloads.Load())
	}
}

// Untranscribed voice messages (empty or whitespace-only transcript) are downloaded, decrypted and forwarded as files.
// Raw PCM is wrapped into a standard mono WAV header; SILK and other containers keep their detected/declared format.
func TestUntranscribedVoiceDownloadsAudioFallback(t *testing.T) {
	r := newRig(t, true)
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	keyRaw64 := base64.StdEncoding.EncodeToString(key)
	keyHex64 := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(key)))

	wechatSilk := append([]byte("\x02#!SILK_V3"), []byte("synthetic-wechat-silk-frames")...)
	rawPCM := bytes.Repeat([]byte{0x01, 0x02}, 160) // 10ms at 16 kHz 16-bit mono
	amr := append([]byte("#!AMR\n"), []byte("synthetic-amr-frames")...)
	ogg := append([]byte("OggS"), []byte("synthetic-ogg-frames")...)
	mp3 := append([]byte("ID3"), []byte("synthetic-mp3-frames")...)

	r.ilink.mu.Lock()
	r.ilink.downloads = map[string][]byte{
		"V_SILK": encryptVoiceFixture(t, wechatSilk, key),
		"V_PCM":  encryptVoiceFixture(t, rawPCM, key),
		"V_AMR":  encryptVoiceFixture(t, amr, key),
		"V_OGG":  encryptVoiceFixture(t, ogg, key),
		"V_MP3":  encryptVoiceFixture(t, mp3, key),
	}
	r.ilink.mu.Unlock()

	// 1) Empty transcript + WeChat SILK (with base64-of-raw key).
	r.ilink.say(owner, "", map[string]any{
		"type": 3,
		"voice_item": map[string]any{
			"encode_type": ilink.VoiceEncodeSILK,
			"sample_rate": 24000,
			"playtime":    2500,
			"media":       map[string]any{"encrypt_query_param": "V_SILK", "aes_key": keyRaw64},
		},
	})
	eventually(t, "untranscribed SILK voice at hub", func() bool { return len(r.hub.got()) == 1 })
	m := r.hub.got()[0]
	if m.Text != "" || len(m.Unsupported) != 0 || len(m.Files) != 1 {
		t.Fatalf("SILK voice handed %+v", m)
	}
	if m.Files[0].Name != "voice.silk" || m.Files[0].Mime != "audio/silk" ||
		m.Files[0].Data != base64.StdEncoding.EncodeToString(wechatSilk) {
		t.Fatalf("SILK voice file = %+v", m.Files[0])
	}

	// 2) Whitespace-only transcript + raw PCM (with base64-of-hex key) wrapped to WAV.
	r.ilink.say(owner, "", map[string]any{
		"type": 3,
		"voice_item": map[string]any{
			"text":            "   \n\t ",
			"encode_type":     ilink.VoiceEncodePCM,
			"sample_rate":     16000,
			"bits_per_sample": 16,
			"media":           map[string]any{"encrypt_query_param": "V_PCM", "aes_key": keyHex64},
		},
	})
	eventually(t, "untranscribed PCM voice at hub", func() bool { return len(r.hub.got()) == 2 })
	m = r.hub.got()[1]
	if m.Text != "" || len(m.Unsupported) != 0 || len(m.Files) != 1 {
		t.Fatalf("PCM voice handed %+v", m)
	}
	if m.Files[0].Name != "voice.wav" || m.Files[0].Mime != "audio/wav" {
		t.Fatalf("PCM voice file metadata = %q %q", m.Files[0].Name, m.Files[0].Mime)
	}
	wavBytes, err := base64.StdEncoding.DecodeString(m.Files[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(wavBytes) != wavHeaderBytes+len(rawPCM) {
		t.Fatalf("WAV length = %d, want %d", len(wavBytes), wavHeaderBytes+len(rawPCM))
	}
	if string(wavBytes[0:4]) != "RIFF" || binary.LittleEndian.Uint32(wavBytes[4:8]) != uint32(36+len(rawPCM)) ||
		string(wavBytes[8:12]) != "WAVE" || string(wavBytes[12:16]) != "fmt " ||
		binary.LittleEndian.Uint16(wavBytes[20:22]) != 1 || binary.LittleEndian.Uint16(wavBytes[22:24]) != 1 ||
		binary.LittleEndian.Uint32(wavBytes[24:28]) != 16000 || binary.LittleEndian.Uint32(wavBytes[28:32]) != 32000 ||
		binary.LittleEndian.Uint16(wavBytes[32:34]) != 2 || binary.LittleEndian.Uint16(wavBytes[34:36]) != 16 ||
		string(wavBytes[36:40]) != "data" || binary.LittleEndian.Uint32(wavBytes[40:44]) != uint32(len(rawPCM)) ||
		!bytes.Equal(wavBytes[wavHeaderBytes:], rawPCM) {
		t.Fatalf("WAV header or payload mismatch: %x", wavBytes[:wavHeaderBytes])
	}

	// 3) Mixed message: text + multiple untranscribed voices in different formats (none silently dropped, distinct names).
	r.ilink.say(owner, "",
		map[string]any{"type": 1, "text_item": map[string]any{"text": "补充几段语音"}},
		map[string]any{"type": 3, "voice_item": map[string]any{"media": map[string]any{"encrypt_query_param": "V_AMR", "aes_key": keyRaw64}}},
		map[string]any{"type": 3, "voice_item": map[string]any{"media": map[string]any{"encrypt_query_param": "V_OGG", "aes_key": keyRaw64}}},
		map[string]any{"type": 3, "voice_item": map[string]any{"media": map[string]any{"encrypt_query_param": "V_MP3", "aes_key": keyRaw64}}},
	)
	eventually(t, "mixed message with multiple voices at hub", func() bool { return len(r.hub.got()) == 3 })
	m = r.hub.got()[2]
	if m.Text != "补充几段语音" || len(m.Unsupported) != 0 || len(m.Files) != 3 {
		t.Fatalf("mixed message handed text=%q files=%d unsupported=%v", m.Text, len(m.Files), m.Unsupported)
	}
	wantFiles := []struct {
		name, mime string
		plain      []byte
	}{
		{"voice.amr", "audio/amr", amr},
		{"voice-2.ogg", "audio/ogg", ogg},
		{"voice-3.mp3", "audio/mpeg", mp3},
	}
	for i, want := range wantFiles {
		got := m.Files[i]
		if got.Name != want.name || got.Mime != want.mime || got.Data != base64.StdEncoding.EncodeToString(want.plain) {
			t.Errorf("voice file %d = (%q, %q), want (%q, %q) with matching payload", i, got.Name, got.Mime, want.name, want.mime)
		}
	}
}

func TestPrepareVoiceAudioFormatDetection(t *testing.T) {
	alreadyWAV, err := pcmToWAV([]byte{1, 2, 3, 4}, 24000, 16)
	if err != nil {
		t.Fatal(err)
	}
	mp3Frame := []byte{0xff, 0xfb, 0x90, 0x00, 0x11, 0x22}
	m4a := append([]byte{0, 0, 0, 20}, []byte("ftypM4A synthetic")...)

	tests := []struct {
		name     string
		md       ilink.Media
		data     []byte
		wantExt  string
		wantMime string
		wantLen  int
	}{
		{name: "standard SILK header", data: []byte("#!SILK_V3payload"), wantExt: ".silk", wantMime: "audio/silk", wantLen: 16},
		{name: "AMR-WB header", data: []byte("#!AMR-WB\nframes"), wantExt: ".amr", wantMime: "audio/amr", wantLen: 15},
		{name: "already WAV even with PCM encode_type is not double wrapped", md: ilink.Media{EncodeType: ilink.VoiceEncodePCM}, data: alreadyWAV, wantExt: ".wav", wantMime: "audio/wav", wantLen: len(alreadyWAV)},
		{name: "PCM defaults to 24kHz 16-bit WAV", md: ilink.Media{EncodeType: ilink.VoiceEncodePCM}, data: []byte{0, 1, 2, 3}, wantExt: ".wav", wantMime: "audio/wav", wantLen: wavHeaderBytes + 4},
		{name: "PCM 8-bit allows odd byte length", md: ilink.Media{EncodeType: ilink.VoiceEncodePCM, SampleRate: 8000, BitsPerSample: 8}, data: []byte{0x80, 0x81, 0x82}, wantExt: ".wav", wantMime: "audio/wav", wantLen: wavHeaderBytes + 3},
		{name: "bare MP3 frame sync when encode_type omitted", data: mp3Frame, wantExt: ".mp3", wantMime: "audio/mpeg", wantLen: len(mp3Frame)},
		{name: "raw PCM starting with FF FB is not misclassified as MP3", md: ilink.Media{EncodeType: ilink.VoiceEncodePCM}, data: mp3Frame, wantExt: ".wav", wantMime: "audio/wav", wantLen: wavHeaderBytes + len(mp3Frame)},
		{name: "M4A ftyp header", data: m4a, wantExt: ".m4a", wantMime: "audio/mp4", wantLen: len(m4a)},
		{name: "omitted encode_type without magic defaults to raw SILK", data: []byte("opaque-silk-bytes"), wantExt: ".silk", wantMime: "audio/silk", wantLen: 17},
		{name: "declared encode_type AMR without header", md: ilink.Media{EncodeType: ilink.VoiceEncodeAMR}, data: []byte("raw-amr"), wantExt: ".amr", wantMime: "audio/amr", wantLen: 7},
		{name: "declared encode_type OGG-Speex without header", md: ilink.Media{EncodeType: ilink.VoiceEncodeOGGSpeex}, data: []byte("raw-ogg"), wantExt: ".ogg", wantMime: "audio/ogg", wantLen: 7},
		{name: "declared encode_type Speex", md: ilink.Media{EncodeType: ilink.VoiceEncodeSpeex}, data: []byte("raw-speex"), wantExt: ".spx", wantMime: "audio/speex", wantLen: 9},
		{name: "declared encode_type ADPCM", md: ilink.Media{EncodeType: ilink.VoiceEncodeADPCM}, data: []byte("raw-adpcm"), wantExt: ".adpcm", wantMime: "audio/adpcm", wantLen: 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ext, mime, payload, err := prepareVoiceAudio(tt.md, tt.data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ext != tt.wantExt || mime != tt.wantMime || len(payload) != tt.wantLen {
				t.Fatalf("got (%q, %q, %d), want (%q, %q, %d)", ext, mime, len(payload), tt.wantExt, tt.wantMime, tt.wantLen)
			}
		})
	}
}

// Download, decryption, validation and format failures degrade cleanly to an unsupported voice notice without leaking
// URLs, keys or payloads into logs.
func TestUntranscribedVoiceFailureDegradationAndLogPrivacy(t *testing.T) {
	r := newRig(t, true)
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	wrongKey, _ := hex.DecodeString("ffeeddccbbaa99887766554433221100")
	key64 := base64.StdEncoding.EncodeToString(key)
	wrongKey64 := base64.StdEncoding.EncodeToString(wrongKey)

	secretQuery := "secret-voice-query-token"
	secretPayload := []byte("\x02#!SILK_V3very-private-audio")

	r.ilink.mu.Lock()
	r.ilink.downloads = map[string][]byte{
		secretQuery:  encryptVoiceFixture(t, secretPayload, key),
		"BAD_BLOCKS": []byte("not-a-multiple-of-16"),
		"EMPTY":      encryptVoiceFixture(t, nil, key),
		"ODD_PCM":    encryptVoiceFixture(t, []byte{1, 2, 3}, key),
		"FEATURE":    encryptVoiceFixture(t, []byte("feature-vector"), key),
	}
	r.ilink.mu.Unlock()

	logs := captureLog(t)

	cases := []struct {
		name string
		item map[string]any
		want string
	}{
		{
			name: "missing media reference",
			item: map[string]any{"type": 3, "voice_item": map[string]any{}},
			want: "一段没有转写的语音",
		},
		{
			name: "missing aes key",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"media": map[string]any{"encrypt_query_param": secretQuery}}},
			want: "一段没下载下来的语音",
		},
		{
			name: "invalid aes key encoding",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"media": map[string]any{"encrypt_query_param": secretQuery, "aes_key": "not-base64!"}}},
			want: "一段没下载下来的语音",
		},
		{
			name: "foreign full_url blocked",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"media": map[string]any{"full_url": "https://evil.example/voice.silk", "aes_key": key64}}},
			want: "一段没下载下来的语音",
		},
		{
			name: "HTTP 404 download failure",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"media": map[string]any{"encrypt_query_param": "missing-on-cdn", "aes_key": key64}}},
			want: "一段没下载下来的语音",
		},
		{
			name: "wrong decryption key bad padding",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"media": map[string]any{"encrypt_query_param": secretQuery, "aes_key": wrongKey64}}},
			want: "一段没下载下来的语音",
		},
		{
			name: "corrupted ciphertext not whole AES blocks",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"media": map[string]any{"encrypt_query_param": "BAD_BLOCKS", "aes_key": key64}}},
			want: "一段没下载下来的语音",
		},
		{
			name: "empty decrypted payload",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"media": map[string]any{"encrypt_query_param": "EMPTY", "aes_key": key64}}},
			want: "一段没下载下来的语音",
		},
		{
			name: "unaligned 16-bit PCM",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"encode_type": ilink.VoiceEncodePCM, "bits_per_sample": 16, "media": map[string]any{"encrypt_query_param": "ODD_PCM", "aes_key": key64}}},
			want: "一段无法处理的语音",
		},
		{
			name: "invalid PCM sample rate",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"encode_type": ilink.VoiceEncodePCM, "sample_rate": 100, "media": map[string]any{"encrypt_query_param": "ODD_PCM", "aes_key": key64}}},
			want: "一段无法处理的语音",
		},
		{
			name: "invalid PCM bit depth",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"encode_type": ilink.VoiceEncodePCM, "bits_per_sample": 24, "media": map[string]any{"encrypt_query_param": "ODD_PCM", "aes_key": key64}}},
			want: "一段无法处理的语音",
		},
		{
			name: "unsupported encode_type feature",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"encode_type": ilink.VoiceEncodeFeature, "media": map[string]any{"encrypt_query_param": "FEATURE", "aes_key": key64}}},
			want: "一段无法处理的语音",
		},
		{
			name: "unknown non-zero encode_type",
			item: map[string]any{"type": 3, "voice_item": map[string]any{"encode_type": 99, "media": map[string]any{"encrypt_query_param": "FEATURE", "aes_key": key64}}},
			want: "一段无法处理的语音",
		},
	}

	for i, tc := range cases {
		r.ilink.say(owner, "", tc.item)
		wantCount := i + 1
		eventually(t, tc.name, func() bool { return len(r.hub.got()) == wantCount })
		got := r.hub.got()[i]
		if len(got.Files) != 0 || len(got.Unsupported) != 1 || got.Unsupported[0] != tc.want {
			t.Errorf("%s: handed files=%d unsupported=%v, want 0 files and [%q]", tc.name, len(got.Files), got.Unsupported, tc.want)
		}
	}

	logText := logs.String()
	// Rejected media URLs log sanitized origin-only diagnostics (scheme/host/port) without path, query, keys, or audio.
	if !strings.Contains(logText, "source=media_url") || !strings.Contains(logText, `host="evil.example"`) {
		t.Errorf("voice failure logs missing sanitized media_url diagnostic: %q", logText)
	}
	for _, secret := range []string{secretQuery, key64, wrongKey64, "voice.silk", "very-private-audio", "https://"} {
		if strings.Contains(logText, secret) {
			t.Errorf("voice failure logs leaked %q: %q", secret, logText)
		}
	}
}

// Voice fallback respects both the 8 MiB per-voice cap (including the WAV header for PCM) and the 40 MiB combined
// message media budget; exhausted budgets skip CDN downloads altogether.
func TestUntranscribedVoiceSizeAndBudgetLimits(t *testing.T) {
	f := newFakeILink(t)
	var voiceDownloads atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/c2c/download" && strings.HasPrefix(r.URL.Query().Get("encrypted_query_param"), "V_") {
			voiceDownloads.Add(1)
		}
		return f.RoundTrip(r)
	})
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	key64 := base64.StdEncoding.EncodeToString(key)

	fileChunk := make([]byte, 8<<20)
	voice5M := append([]byte("\x02#!SILK_V3"), make([]byte, (5<<20)-10)...)
	voiceOver8M := append([]byte("\x02#!SILK_V3"), make([]byte, (ilink.MaxMedia+1)-10)...)
	pcmExact8M := make([]byte, ilink.MaxMedia)

	f.mu.Lock()
	f.downloads = map[string][]byte{
		"F_8M":     encryptVoiceFixture(t, fileChunk, key),
		"V_5M":     encryptVoiceFixture(t, voice5M, key),
		"V_OVER8M": encryptVoiceFixture(t, voiceOver8M, key),
		"V_PCM8M":  encryptVoiceFixture(t, pcmExact8M, key),
	}
	f.mu.Unlock()

	c, err := ilink.New(ilink.Options{BaseURL: "https://ilinkai.weixin.qq.com", Token: "BT", Agent: "Persona/test",
		HTTP: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{}
	ctx := context.Background()

	// 1) Voice > MaxMedia (8 MiB + 1).
	got := bot.compose(ctx, c, owner, ilink.Message{Media: []ilink.Media{
		{Kind: ilink.MediaVoice, Query: "V_OVER8M", KeyB64: key64, EncodeType: ilink.VoiceEncodeSILK},
	}})
	if len(got.Files) != 0 || len(got.Unsupported) != 1 || got.Unsupported[0] != "一段太大的语音" {
		t.Fatalf("voice > MaxMedia handed files=%d unsupported=%v", len(got.Files), got.Unsupported)
	}

	// 2) Raw PCM at exact MaxMedia: +44 byte WAV header exceeds MaxMedia.
	got = bot.compose(ctx, c, owner, ilink.Message{Media: []ilink.Media{
		{Kind: ilink.MediaVoice, Query: "V_PCM8M", KeyB64: key64, EncodeType: ilink.VoiceEncodePCM, SampleRate: 24000, BitsPerSample: 16},
	}})
	if len(got.Files) != 0 || len(got.Unsupported) != 1 || got.Unsupported[0] != "一段太大的语音" {
		t.Fatalf("PCM + WAV header > MaxMedia handed files=%d unsupported=%v", len(got.Files), got.Unsupported)
	}

	// 3) Budget already exhausted by 5 x 8 MiB files (= 40 MiB): voice download is skipped without network call.
	before := voiceDownloads.Load()
	media := make([]ilink.Media, 0, 6)
	for range 5 {
		media = append(media, ilink.Media{Kind: ilink.MediaFile, Query: "F_8M", KeyB64: key64, FileName: "chunk.bin", Size: int64(len(fileChunk))})
	}
	media = append(media, ilink.Media{Kind: ilink.MediaVoice, Query: "V_5M", KeyB64: key64, EncodeType: ilink.VoiceEncodeSILK})
	got = bot.compose(ctx, c, owner, ilink.Message{Media: media})
	if len(got.Files) != 5 || len(got.Unsupported) != 1 || got.Unsupported[0] != "一段太大的语音" {
		t.Fatalf("exhausted budget handed files=%d unsupported=%v", len(got.Files), got.Unsupported)
	}
	if voiceDownloads.Load() != before {
		t.Fatalf("exhausted budget still downloaded voice %d times", voiceDownloads.Load()-before)
	}

	// 4) Budget partially used (36 MiB left -> 4 MiB): a 5 MiB voice fits MaxMedia (8 MiB) but exceeds remaining budget.
	file36M := make([]byte, 36<<20)
	f.mu.Lock()
	f.downloads["F_36M"] = encryptVoiceFixture(t, file36M, key)
	f.mu.Unlock()
	got = bot.compose(ctx, c, owner, ilink.Message{Media: []ilink.Media{
		{Kind: ilink.MediaFile, Query: "F_36M", KeyB64: key64, FileName: "big.bin", Size: int64(len(file36M))},
		{Kind: ilink.MediaVoice, Query: "V_5M", KeyB64: key64, EncodeType: ilink.VoiceEncodeSILK},
	}})
	if len(got.Files) != 1 || len(got.Unsupported) != 1 || got.Unsupported[0] != "一段太大的语音" {
		t.Fatalf("partial remaining budget handed files=%d unsupported=%v", len(got.Files), got.Unsupported)
	}
}

// Untranscribed voice messages still index a sanitized [语音] summary for svr_id quotes without storing keys or audio.
func TestUntranscribedVoiceQuoteRestoreStaysSanitized(t *testing.T) {
	r := newRig(t, true)
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	key64 := base64.StdEncoding.EncodeToString(key)
	silk := append([]byte("\x02#!SILK_V3"), []byte("quote-secret-audio-bytes")...)
	r.ilink.mu.Lock()
	r.ilink.downloads = map[string][]byte{"V_QUOTE": encryptVoiceFixture(t, silk, key)}
	r.ilink.mu.Unlock()

	queueQuoteMessage(r, 9501, map[string]any{
		"type": 3,
		"voice_item": map[string]any{
			"encode_type": ilink.VoiceEncodeSILK,
			"media":       map[string]any{"encrypt_query_param": "V_QUOTE", "aes_key": key64},
		},
	})
	eventually(t, "untranscribed voice message at hub", func() bool { return len(r.hub.got()) == 1 })
	if len(r.hub.got()[0].Files) != 1 || r.hub.got()[0].Files[0].Name != "voice.silk" {
		t.Fatalf("voice message = %+v", r.hub.got()[0])
	}

	queueQuoteMessage(r, 9502, map[string]any{
		"type":      1,
		"text_item": map[string]any{"text": "刚才那段语音帮我总结一下"},
		"ref_msg":   map[string]any{"svr_id": "9501"},
	})
	eventually(t, "quoted untranscribed voice at hub", func() bool { return len(r.hub.got()) == 2 })
	if got := r.hub.got()[1]; got.Quote != "[语音]" {
		t.Fatalf("restored voice quote = %q, want [语音]", got.Quote)
	}
	raw, err := os.ReadFile(filepath.Join(r.bot.opt.DataDir, quoteFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"V_QUOTE", key64, "quote-secret-audio-bytes", base64.StdEncoding.EncodeToString(silk)} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("quote index leaked %q", forbidden)
		}
	}
}

// Concurrent voice fallback calls must be race-free.
func TestUntranscribedVoiceConcurrentCompose(t *testing.T) {
	f := newFakeILink(t)
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	key64 := base64.StdEncoding.EncodeToString(key)
	silk := append([]byte("\x02#!SILK_V3"), []byte("concurrent-silk")...)
	pcm := bytes.Repeat([]byte{0x10, 0x20}, 80)

	f.mu.Lock()
	f.downloads = map[string][]byte{
		"V_SILK": encryptVoiceFixture(t, silk, key),
		"V_PCM":  encryptVoiceFixture(t, pcm, key),
	}
	f.mu.Unlock()

	c, err := ilink.New(ilink.Options{BaseURL: "https://ilinkai.weixin.qq.com", Token: "BT", Agent: "Persona/test",
		HTTP: &http.Client{Transport: f}})
	if err != nil {
		t.Fatal(err)
	}
	bot := New(Options{DataDir: t.TempDir()}, nil)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range 8 {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				got := bot.compose(ctx, c, owner, ilink.Message{
					SvrID: "c-silk",
					Media: []ilink.Media{{Kind: ilink.MediaVoice, Query: "V_SILK", KeyB64: key64}},
				})
				if len(got.Files) != 1 || got.Files[0].Mime != "audio/silk" {
					t.Errorf("worker %d silk = %+v", i, got)
				}
			case 1:
				got := bot.compose(ctx, c, owner, ilink.Message{
					SvrID: "c-pcm",
					Media: []ilink.Media{{Kind: ilink.MediaVoice, Query: "V_PCM", KeyB64: key64, EncodeType: ilink.VoiceEncodePCM}},
				})
				if len(got.Files) != 1 || got.Files[0].Mime != "audio/wav" {
					t.Errorf("worker %d pcm = %+v", i, got)
				}
			default:
				got := bot.compose(ctx, c, owner, ilink.Message{
					SvrID: "c-miss",
					Media: []ilink.Media{{Kind: ilink.MediaVoice, Query: "missing", KeyB64: key64}},
				})
				if len(got.Unsupported) != 1 || got.Unsupported[0] != "一段没下载下来的语音" {
					t.Errorf("worker %d missing = %+v", i, got)
				}
			}
		}()
	}
	wg.Wait()
}

// A nil client never panics when composing an untranscribed voice message.
func TestComposeUntranscribedVoiceWithNilClient(t *testing.T) {
	got := (&Bot{}).compose(context.Background(), nil, owner, ilink.Message{
		ID:    "message:1",
		Media: []ilink.Media{{Kind: ilink.MediaVoice, Query: "V_SILK", KeyB64: "abc"}},
	})
	if len(got.Files) != 0 || len(got.Unsupported) != 1 || got.Unsupported[0] != "一段没有转写的语音" {
		t.Fatalf("nil client handed %+v", got)
	}
}

// Explicit full_url on a Tencent HTTPS host works for untranscribed voice download.
func TestUntranscribedVoiceFullURL(t *testing.T) {
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	key64 := base64.StdEncoding.EncodeToString(key)
	silk := append([]byte("\x02#!SILK_V3"), []byte("full-url-silk")...)
	enc := encryptVoiceFixture(t, silk, key)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/voice-blob" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(enc)
	}))
	t.Cleanup(srv.Close)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme = "http"
		r2.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		r2.Host = r2.URL.Host
		return http.DefaultTransport.RoundTrip(r2)
	})
	c, err := ilink.New(ilink.Options{BaseURL: "https://ilinkai.weixin.qq.com", Token: "BT", Agent: "Persona/test",
		HTTP: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	for _, fullURL := range []string{
		"https://novac2c.cdn.weixin.qq.com/v2/voice-blob",
		"https://novac2c.cdn.wechat.com/v2/voice-blob",
	} {
		got := (&Bot{}).compose(context.Background(), c, owner, ilink.Message{
			ID: "message:fullurl",
			Media: []ilink.Media{{
				Kind:   ilink.MediaVoice,
				URL:    fullURL,
				KeyB64: key64,
			}},
		})
		if len(got.Files) != 1 || got.Files[0].Name != "voice.silk" || got.Files[0].Mime != "audio/silk" ||
			got.Files[0].Data != base64.StdEncoding.EncodeToString(silk) {
			t.Fatalf("full_url voice (%s) handed %+v", fullURL, got)
		}
	}
}

// Verify wire.ChannelMessage with an untranscribed voice file is non-empty and passes schema validation.
func TestUntranscribedVoiceMessageIsValidWire(t *testing.T) {
	msg := wire.ChannelMessage{
		ID: "message:voice-1",
		Files: []wire.ChannelBlob{{
			Name: "voice.silk",
			Mime: "audio/silk",
			Data: base64.StdEncoding.EncodeToString([]byte("\x02#!SILK_V3test")),
		}},
	}
	if empty(msg) {
		t.Fatal("voice file message treated as empty")
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.ValidateJSON("ChannelMessage", raw); err != nil {
		t.Fatalf("wire validation failed: %v", err)
	}
}
