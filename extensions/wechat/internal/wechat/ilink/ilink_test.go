package ilink

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fake routes every request, whatever its host, to a handler; it records the hosts asked for.
type fake struct {
	t       *testing.T
	srv     *httptest.Server
	handler func(w http.ResponseWriter, r *http.Request, body map[string]any)
	hosts   []string
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body map[string]any)) *fake {
	f := &fake{t: t, handler: h}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" { // a node's port probe (node/ports.go), not this test's client
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b)) // a CDN upload's handler reads the picture
		if len(b) > 0 && r.Header.Get("Content-Type") == "application/json" {
			if err := json.Unmarshal(b, &body); err != nil {
				t.Errorf("request body is not JSON: %s", b)
			}
		}
		f.handler(w, r, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) RoundTrip(r *http.Request) (*http.Response, error) {
	f.hosts = append(f.hosts, r.URL.Host)
	u, _ := url.Parse(f.srv.URL)
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host, r2.Host = u.Scheme, u.Host, u.Host
	return http.DefaultTransport.RoundTrip(r2)
}

func (f *fake) client(token, base string) *Client {
	c, err := New(Options{BaseURL: base, Token: token, Agent: "Persona/1 bad<agent>", HTTP: &http.Client{Transport: f}})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestErrorType(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "dns", err: &Error{Code: CodeNetwork, err: &url.Error{Err: &net.DNSError{Name: "example.invalid"}}}, want: "dns"},
		{name: "tcp", err: &Error{Code: CodeNetwork, err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}}, want: "tcp"},
		{name: "tls", err: &Error{Code: CodeNetwork, err: &tls.RecordHeaderError{Msg: "bad record"}}, want: "tls"},
		{name: "network timeout", err: &Error{Code: CodeNetwork, err: &net.OpError{Err: context.DeadlineExceeded}}, want: "timeout"},
		{name: "client timeout", err: &Error{Code: CodeTimeout}, want: "timeout"},
		{name: "cancelled", err: context.Canceled, want: "aborted"},
		{name: "aborted", err: &Error{Code: CodeAborted}, want: "aborted"},
		{name: "http", err: &Error{Code: CodeHTTP}, want: "http"},
		{name: "other network", err: &Error{Code: CodeNetwork, err: errors.New("unknown")}, want: "network"},
		{name: "other", err: errors.New("unknown"), want: "unknown"},
		{name: "nil", err: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ErrorType(tt.err); got != tt.want {
				t.Errorf("ErrorType(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestValidateBaseURL(t *testing.T) {
	for _, ok := range []string{
		"https://ilinkai.weixin.qq.com",
		"https://ilinkai.weixin.qq.com/",
		"https://weixin.qq.com:443",
		"https://ilinkai.wechat.com",
		"https://ilinkai.wechat.com/",
		"https://sg.ilinkai.wechat.com/",
		"https://wechat.com",
		"https://wechat.com:443",
		"https://ILINKAI.WECHAT.COM/",
	} {
		if _, err := ValidateBaseURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://ilinkai.weixin.qq.com",
		"http://ilinkai.wechat.com",
		"https://evil.com",
		"https://weixin.qq.com.evil.com",
		"https://wechat.com.evil.com",
		"https://evilweixin.qq.com",
		"https://evilwechat.com",
		"https://.weixin.qq.com",
		"https://.wechat.com",
		"https://a..wechat.com",
		"https://-bad.wechat.com",
		"https://ilinkai.wechat.com.",
		"https://u:p@ilinkai.weixin.qq.com",
		// Build userinfo fixtures via url.URL so static privacy scans do not mistake URL credentials for email addresses.
		(&url.URL{Scheme: "https", User: url.UserPassword("u", "p"), Host: "ilinkai.wechat.com"}).String(),
		"https://ilinkai.weixin.qq.com:8443",
		"https://ilinkai.wechat.com:8443",
		"https://ilinkai.weixin.qq.com/x",
		"https://ilinkai.wechat.com/x",
		"https://ilinkai.weixin.qq.com/?a=1",
		"https://ilinkai.wechat.com/?a=1",
		"https://ilinkai.wechat.com/?",
		"https://ilinkai.wechat.com/#frag",
		"ilinkai.wechat.com",
		"://bad?token=secret",
	} {
		if _, err := ValidateBaseURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestValidateBaseURLDiagnostics(t *testing.T) {
	raw := (&url.URL{
		Scheme:   "https",
		User:     url.UserPassword("user", "supersecret"),
		Host:     "ilinkai.wechat.com:8443",
		Path:     "/prefix",
		RawQuery: "bot_token=BOT_SECRET&qrcode=QR_SECRET&user=OWNER_ID",
		Fragment: "FRAG_SECRET",
	}).String()
	_, err := ValidateBaseURL(raw)
	if err == nil || CodeOf(err) != CodeProtocol {
		t.Fatalf("expected protocol error, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{`scheme="https"`, `host="ilinkai.wechat.com"`, `port="8443"`, `path="/prefix"`, "userinfo=redacted", "query=redacted", "fragment=redacted"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
	for _, forbidden := range []string{"user:", "supersecret", "bot_token", "BOT_SECRET", "qrcode", "QR_SECRET", "OWNER_ID", "FRAG_SECRET"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("leaked %q in %q", forbidden, msg)
		}
	}

	_, err = ValidateBaseURL("://bad?bot_token=UNPARSEABLE_SECRET")
	if err == nil || !strings.Contains(err.Error(), "unparseable") || strings.Contains(err.Error(), "UNPARSEABLE_SECRET") {
		t.Fatalf("unexpected unparseable diagnostic: %v", err)
	}

	longPath := "/" + strings.Repeat("a", 100)
	_, err = ValidateBaseURL("https://ilinkai.wechat.com" + longPath)
	if err == nil || !strings.Contains(err.Error(), `path="/`+strings.Repeat("a", diagMaxPath-1)+`..."`) {
		t.Fatalf("expected truncated path diagnostic, got %v", err)
	}
}

func TestSanitizeAgentAndVersion(t *testing.T) {
	if got := sanitizeAgent("Persona/1 bad<agent> x/y+z"); got != "Persona/1 x/y+z" {
		t.Errorf("agent %q", got)
	}
	if got := sanitizeAgent("nothing"); got != "Persona/unknown" {
		t.Errorf("empty agent %q", got)
	}
	if got := versionNumber("2.4.6"); got != 2<<16|4<<8|6 {
		t.Errorf("version %d", got)
	}
}

func TestQRAuthorization(t *testing.T) {
	polls := 0
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			if r.URL.Query().Get("bot_type") != "3" || r.Header.Get("Authorization") != "" {
				t.Errorf("begin: %s %q", r.URL, r.Header.Get("Authorization"))
			}
			if toks := body["local_token_list"].([]any); len(toks) != 10 || toks[9] != "t11" {
				t.Errorf("local tokens %v", toks)
			}
			writeJSON(w, map[string]any{"qrcode": "QC", "qrcode_img_content": "https://liteapp.weixin.qq.com/q/abc"})
		case "/ilink/bot/get_qrcode_status":
			polls++
			if r.URL.Query().Get("qrcode") != "QC" {
				t.Errorf("poll %s", r.URL)
			}
			switch polls {
			case 1:
				writeJSON(w, map[string]any{"status": "scaned_but_redirect", "redirect_host": "sh.ilinkai.weixin.qq.com"})
			case 2:
				if r.URL.Query().Get("verify_code") != "123456" {
					t.Errorf("verify code %s", r.URL)
				}
				writeJSON(w, map[string]any{"status": "confirmed", "bot_token": "BT", "ilink_bot_id": "bot@im", "ilink_user_id": "owner@im", "baseurl": "https://ilinkai.weixin.qq.com"})
			}
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	})
	c := f.client("", "")
	toks := []string{"t0", "t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9", "t10", "t11"}
	q, err := c.BeginQR(context.Background(), toks[:12])
	if err != nil || q.Code != "QC" || !strings.HasPrefix(q.Content, "https://liteapp.weixin.qq.com/") {
		t.Fatalf("begin %+v %v", q, err)
	}
	st, err := c.PollQR(context.Background(), q.Code, "", "", time.Second)
	if err != nil || st.State != QRRedirect || st.RedirectBase != "https://sh.ilinkai.weixin.qq.com/" {
		t.Fatalf("redirect %+v %v", st, err)
	}
	st, err = c.PollQR(context.Background(), q.Code, "123456", st.RedirectBase, time.Second)
	if err != nil || st.State != QRConfirmed || st.Credentials != (Credentials{Token: "BT", BotID: "bot@im", UserID: "owner@im", BaseURL: "https://ilinkai.weixin.qq.com/"}) {
		t.Fatalf("confirmed %+v %v", st, err)
	}
	if f.hosts[0] != "ilinkai.weixin.qq.com" || f.hosts[2] != "sh.ilinkai.weixin.qq.com" {
		t.Errorf("hosts %v", f.hosts)
	}
}

func TestQRAuthorizationInternationalWeChat(t *testing.T) {
	polls := 0
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			writeJSON(w, map[string]any{"qrcode": "QC-INTL", "qrcode_img_content": "https://liteapp.wechat.com/q/intl"})
		case "/ilink/bot/get_qrcode_status":
			polls++
			switch polls {
			case 1:
				writeJSON(w, map[string]any{"status": "scaned_but_redirect", "redirect_host": "ilinkai.wechat.com"})
			case 2:
				writeJSON(w, map[string]any{
					"status":        "confirmed",
					"bot_token":     "BT-INTL",
					"ilink_bot_id":  "bot@im",
					"ilink_user_id": "owner@im",
					"baseurl":       "https://ilinkai.wechat.com/",
				})
			}
		case "/ilink/bot/getupdates":
			writeJSON(w, map[string]any{"ret": 0, "get_updates_buf": "C1", "msgs": []any{}})
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	})
	c := f.client("", "")
	q, err := c.BeginQR(context.Background(), nil)
	if err != nil || q.Code != "QC-INTL" || q.Content != "https://liteapp.wechat.com/q/intl" {
		t.Fatalf("begin %+v %v", q, err)
	}
	st, err := c.PollQR(context.Background(), q.Code, "", "", time.Second)
	if err != nil || st.State != QRRedirect || st.RedirectBase != "https://ilinkai.wechat.com/" {
		t.Fatalf("redirect %+v %v", st, err)
	}
	st, err = c.PollQR(context.Background(), q.Code, "", st.RedirectBase, time.Second)
	want := Credentials{Token: "BT-INTL", BotID: "bot@im", UserID: "owner@im", BaseURL: "https://ilinkai.wechat.com/"}
	if err != nil || st.State != QRConfirmed || st.Credentials != want {
		t.Fatalf("confirmed %+v %v", st, err)
	}
	intlClient := f.client(st.Credentials.Token, st.Credentials.BaseURL)
	if _, err := intlClient.GetUpdates(context.Background(), ""); err != nil {
		t.Fatalf("getupdates on international base URL: %v", err)
	}
	if len(f.hosts) != 4 || f.hosts[0] != "ilinkai.weixin.qq.com" || f.hosts[1] != "ilinkai.weixin.qq.com" ||
		f.hosts[2] != "ilinkai.wechat.com" || f.hosts[3] != "ilinkai.wechat.com" {
		t.Errorf("hosts %v", f.hosts)
	}
}

func TestQRRejectsForeignContentAndRedirect(t *testing.T) {
	pollStep := 0
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.URL.Path == "/ilink/bot/get_bot_qrcode" {
			writeJSON(w, map[string]any{"qrcode": "QC", "qrcode_img_content": "https://evil.example/q/QR_SECRET_VALUE?k=v"})
			return
		}
		pollStep++
		switch pollStep {
		case 1:
			writeJSON(w, map[string]any{"status": "scaned_but_redirect", "redirect_host": "evil.example"})
		case 2:
			writeJSON(w, map[string]any{"status": "scaned_but_redirect", "redirect_host": "https://ilinkai.wechat.com:8443/p?token=REDIR_SECRET"})
		case 3:
			writeJSON(w, map[string]any{"status": "scaned_but_redirect", "redirect_host": "https://evil.example/p/REDIR_SECRET"})
		case 4:
			writeJSON(w, map[string]any{"status": "scaned_but_redirect", "redirect_host": "evil.example/p/REDIR_SECRET?token=SECRET_Q"})
		case 5:
			writeJSON(w, map[string]any{
				"status":        "confirmed",
				"bot_token":     "BOT_TOKEN_SECRET",
				"ilink_bot_id":  "bot@im",
				"ilink_user_id": "OWNER_USER_SECRET@im",
				"baseurl": (&url.URL{
					Scheme:   "https",
					User:     url.UserPassword("u", "pass"),
					Host:     "evilwechat.com:8443",
					Path:     "/v1",
					RawQuery: "token=QUERY_SECRET",
					Fragment: "FRAG_SECRET",
				}).String(),
			})
		}
	})
	c := f.client("", "")
	if _, err := c.BeginQR(context.Background(), nil); CodeOf(err) != CodeBadResponse ||
		!strings.Contains(err.Error(), "source=qrcode_img_content") ||
		!strings.Contains(err.Error(), `host="evil.example"`) ||
		strings.Contains(err.Error(), "QR_SECRET_VALUE") ||
		strings.Contains(err.Error(), "k=v") {
		t.Errorf("foreign QR content: %v", err)
	}
	if _, err := c.PollQR(context.Background(), "QC", "", "", time.Second); CodeOf(err) != CodeProtocol ||
		!strings.Contains(err.Error(), "source=redirect_host") ||
		!strings.Contains(err.Error(), `host="evil.example"`) {
		t.Errorf("foreign redirect: %v", err)
	}
	if _, err := c.PollQR(context.Background(), "QC", "", "", time.Second); CodeOf(err) != CodeBadResponse ||
		!strings.Contains(err.Error(), "source=redirect_host") ||
		!strings.Contains(err.Error(), `scheme="https"`) ||
		!strings.Contains(err.Error(), `host="ilinkai.wechat.com"`) ||
		!strings.Contains(err.Error(), `port="8443"`) ||
		strings.Contains(err.Error(), "path=") ||
		strings.Contains(err.Error(), "/p") ||
		strings.Contains(err.Error(), "REDIR_SECRET") {
		t.Errorf("malformed redirect: %v", err)
	}
	if _, err := c.PollQR(context.Background(), "QC", "", "", time.Second); CodeOf(err) != CodeBadResponse ||
		!strings.Contains(err.Error(), "source=redirect_host") ||
		!strings.Contains(err.Error(), `scheme="https"`) ||
		!strings.Contains(err.Error(), `host="evil.example"`) ||
		strings.Contains(err.Error(), "path=") ||
		strings.Contains(err.Error(), "/p") ||
		strings.Contains(err.Error(), "REDIR_SECRET") {
		t.Errorf("malformed redirect with path secret: %v", err)
	}
	if _, err := c.PollQR(context.Background(), "QC", "", "", time.Second); CodeOf(err) != CodeBadResponse ||
		!strings.Contains(err.Error(), "source=redirect_host") ||
		!strings.Contains(err.Error(), `scheme=""`) ||
		!strings.Contains(err.Error(), `host="evil.example"`) ||
		strings.Contains(err.Error(), "path=") ||
		strings.Contains(err.Error(), "/p") ||
		strings.Contains(err.Error(), "REDIR_SECRET") ||
		strings.Contains(err.Error(), "SECRET_Q") ||
		strings.Contains(err.Error(), "token=") {
		t.Errorf("schemeless redirect with path/query secret: %v", err)
	}
	if _, err := c.PollQR(context.Background(), "QC", "", "", time.Second); CodeOf(err) != CodeProtocol ||
		!strings.Contains(err.Error(), "source=baseurl") ||
		!strings.Contains(err.Error(), `scheme="https"`) ||
		!strings.Contains(err.Error(), `host="evilwechat.com"`) ||
		!strings.Contains(err.Error(), `port="8443"`) ||
		!strings.Contains(err.Error(), `path="/v1"`) ||
		!strings.Contains(err.Error(), "userinfo=redacted") ||
		!strings.Contains(err.Error(), "query=redacted") ||
		!strings.Contains(err.Error(), "fragment=redacted") ||
		strings.Contains(err.Error(), "BOT_TOKEN_SECRET") ||
		strings.Contains(err.Error(), "OWNER_USER_SECRET") ||
		strings.Contains(err.Error(), "QUERY_SECRET") ||
		strings.Contains(err.Error(), "FRAG_SECRET") ||
		strings.Contains(err.Error(), "pass") {
		t.Errorf("foreign confirmed baseurl: %v", err)
	}
}

func TestPollQRTimeoutIsWaiting(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		<-r.Context().Done()
	})
	st, err := f.client("", "").PollQR(context.Background(), "QC", "", "", 50*time.Millisecond)
	if err != nil || st.State != QRWaiting {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestGetUpdates(t *testing.T) {
	var calls int
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		calls++
		if r.Header.Get("Authorization") != "Bearer BT" || r.Header.Get("AuthorizationType") != "ilink_bot_token" ||
			r.Header.Get("iLink-App-Id") != "bot" || r.Header.Get("X-WECHAT-UIN") == "" {
			t.Errorf("headers %v", r.Header)
		}
		bi := body["base_info"].(map[string]any)
		if bi["bot_agent"] != "Persona/1" || bi["channel_version"] != ClientVersion {
			t.Errorf("base_info %v", bi)
		}
		if calls == 2 {
			writeJSON(w, map[string]any{"errcode": -14})
			return
		}
		if body["get_updates_buf"] != "C0" {
			t.Errorf("cursor %v", body["get_updates_buf"])
		}
		writeJSON(w, map[string]any{"ret": 0, "get_updates_buf": "C1", "longpolling_timeout_ms": 25000, "msgs": []any{
			map[string]any{"message_id": 42, "from_user_id": "owner@im", "context_token": "CT", "message_type": 1, "create_time_ms": 1000,
				"item_list": []any{
					map[string]any{"type": 1, "text_item": map[string]any{"text": "你好"},
						"ref_msg": map[string]any{"title": "引用", "message_item": map[string]any{"type": 1, "text_item": map[string]any{"text": "原文"}}}},
					map[string]any{"type": 3, "voice_item": map[string]any{"text": "语音转写", "media": map[string]any{"encrypt_query_param": "q", "aes_key": "k"}}},
					map[string]any{"type": 2, "image_item": map[string]any{"aeskey": "00112233445566778899aabbccddeeff", "media": map[string]any{"full_url": "https://novac2c.cdn.weixin.qq.com/x"}}},
					map[string]any{"type": 4, "file_item": map[string]any{"file_name": "report.pdf", "len": "123", "media": map[string]any{"encrypt_query_param": "fq", "aes_key": "file-key"}}},
					map[string]any{"type": 5, "video_item": map[string]any{"media": map[string]any{"encrypt_query_param": "vq", "aes_key": "video-key"}}},
				}},
			map[string]any{"client_id": "c-1", "from_user_id": "x", "context_token": "CT2", "item_list": []any{}},
			map[string]any{"message_id": "7", "from_user_id": "bot@im", "context_token": "CT", "message_type": 2},
			map[string]any{"seq": 9, "from_user_id": "y"},
		}})
	})
	c := f.client("BT", "https://ilinkai.weixin.qq.com")
	p, err := c.GetUpdates(context.Background(), "C0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Cursor != "C1" || p.Timeout != 25*time.Second || len(p.Messages) != 2 {
		t.Fatalf("%+v", p)
	}
	m := p.Messages[0]
	if m.ID != "message:42" || m.From != "owner@im" || m.ContextToken != "CT" || m.Text != "你好\n语音转写" || m.Quote != "引用\n原文" || m.At != 1000 {
		t.Errorf("message %+v", m)
	}
	if len(m.Media) != 4 || m.Media[0].Kind != MediaVoice || m.Media[1].Kind != MediaImage || m.Media[1].KeyHex == "" ||
		m.Media[2] != (Media{Kind: MediaFile, Query: "fq", KeyB64: "file-key", FileName: "report.pdf", Size: 123}) ||
		m.Media[3] != (Media{Kind: MediaVideo, Query: "vq", KeyB64: "video-key"}) {
		t.Errorf("media %+v", m.Media)
	}
	if p.Messages[1].ID != "client:c-1" {
		t.Errorf("client id %+v", p.Messages[1])
	}
	if _, err := c.GetUpdates(context.Background(), "C1"); CodeOf(err) != CodeReplaced || Retryable(err) {
		t.Errorf("replaced: %v", err)
	}
}

func TestDecodeVoiceMetadataAndTranscriptFlag(t *testing.T) {
	m := wireMessage{
		MessageID: json.RawMessage(`1001`), FromUserID: "owner@im", ContextToken: "CT",
		Items: []wireItem{
			{
				Type: itemVoice,
				VoiceItem: &struct {
					Media         *wireMedia `json:"media"`
					EncodeType    int        `json:"encode_type"`
					BitsPerSample int        `json:"bits_per_sample"`
					SampleRate    int        `json:"sample_rate"`
					PlayTime      int        `json:"playtime"`
					Text          string     `json:"text"`
				}{
					Media:         &wireMedia{EncryptQueryParam: "vq1", AESKey: "vk1"},
					EncodeType:    VoiceEncodeSILK,
					BitsPerSample: 16,
					SampleRate:    24000,
					PlayTime:      3200,
					Text:          "转写文本",
				},
			},
			{
				Type: itemVoice,
				VoiceItem: &struct {
					Media         *wireMedia `json:"media"`
					EncodeType    int        `json:"encode_type"`
					BitsPerSample int        `json:"bits_per_sample"`
					SampleRate    int        `json:"sample_rate"`
					PlayTime      int        `json:"playtime"`
					Text          string     `json:"text"`
				}{
					Media:         &wireMedia{EncryptQueryParam: "vq2", AESKey: "vk2"},
					EncodeType:    VoiceEncodePCM,
					BitsPerSample: 16,
					SampleRate:    16000,
					PlayTime:      1500,
					Text:          "  \t\n ",
				},
			},
		},
	}
	got, ok := decode(m)
	if !ok {
		t.Fatal("decode failed")
	}
	if got.Text != "转写文本" {
		t.Fatalf("text = %q, want only non-whitespace transcript", got.Text)
	}
	if len(got.Media) != 2 {
		t.Fatalf("media count = %d, want 2", len(got.Media))
	}
	if !got.Media[0].Transcribed || got.Media[0].EncodeType != VoiceEncodeSILK || got.Media[0].SampleRate != 24000 ||
		got.Media[0].BitsPerSample != 16 || got.Media[0].PlayTime != 3200 {
		t.Errorf("transcribed voice media = %+v", got.Media[0])
	}
	if got.Media[1].Transcribed || got.Media[1].EncodeType != VoiceEncodePCM || got.Media[1].SampleRate != 16000 ||
		got.Media[1].BitsPerSample != 16 || got.Media[1].PlayTime != 1500 {
		t.Errorf("untranscribed voice media = %+v", got.Media[1])
	}
}

func TestDownloadVoiceSizeLimits(t *testing.T) {
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	atLimit, err := EncryptECB(make([]byte, MaxMedia), key)
	if err != nil {
		t.Fatal(err)
	}
	overLimit, err := EncryptECB(make([]byte, MaxMedia+1), key)
	if err != nil {
		t.Fatal(err)
	}
	downloads := map[string][]byte{"at": atLimit, "over": overLimit}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		_, _ = w.Write(downloads[r.URL.Query().Get("encrypted_query_param")])
	})
	c := f.client("BT", "")
	key64 := base64.StdEncoding.EncodeToString(key)

	got, err := c.Download(context.Background(), Media{Kind: MediaVoice, Query: "at", KeyB64: key64})
	if err != nil || len(got) != MaxMedia {
		t.Fatalf("voice at MaxMedia: len=%d err=%v", len(got), err)
	}
	got, err = c.Download(context.Background(), Media{Kind: MediaVoice, Query: "over", KeyB64: key64})
	if err == nil || CodeOf(err) != CodeBadResponse || !errors.Is(err, ErrMediaTooLarge) || got != nil {
		t.Fatalf("voice over MaxMedia: len=%d err=%v, want ErrMediaTooLarge", len(got), err)
	}
}

func TestDecodeSvrIDAndIncompleteQuote(t *testing.T) {
	m := wireMessage{MessageID: json.RawMessage(`9007199254740993`), FromUserID: "owner@im", ContextToken: "CT", Items: []wireItem{{
		Type: itemText, TextItem: &struct {
			Text string `json:"text"`
		}{Text: "reply"},
	}}}
	m.Items[0].RefMsg = &struct {
		Title       string           `json:"title"`
		MessageItem *wireItem        `json:"message_item"`
		SvrID       json.RawMessage  `json:"svr_id"`
		PartialText *wirePartialText `json:"partial_text"`
	}{Title: "title", SvrID: json.RawMessage(`"4242"`)}
	got, ok := decode(m)
	if !ok || got.SvrID != "9007199254740993" || got.QuoteID != "4242" || got.Quote != "title" {
		t.Fatalf("decoded ID-only quote = %+v, %v", got, ok)
	}
	m.Items[0].RefMsg.MessageItem = &wireItem{Type: itemText, TextItem: &struct {
		Text string `json:"text"`
	}{Text: "inline original"}}
	got, ok = decode(m)
	if !ok || got.QuoteID != "" || got.Quote != "title\ninline original" {
		t.Fatalf("decoded inline quote = %+v, %v", got, ok)
	}
	m.Items[0].RefMsg.MessageItem = nil
	m.Items[0].RefMsg.SvrID = json.RawMessage(`9007199254740994`)
	got, ok = decode(m)
	if !ok || got.QuoteID != "9007199254740994" {
		t.Fatalf("decoded numeric svr_id = %+v, %v", got, ok)
	}
}

func TestSendTextWithIDKeepsLargeServerID(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":0,"message_id":18446744073709551615}`))
	})
	id, err := f.client("BT", "").SendTextWithID(context.Background(), "owner@im", "CT", "cid", "回答")
	if err != nil || id != "18446744073709551615" {
		t.Fatalf("server id = %q, error = %v", id, err)
	}
}

func TestGetUpdatesTimeoutKeepsCursor(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, map[string]any{"ret": -1})
	})
	if _, err := f.client("BT", "").GetUpdates(context.Background(), "C"); CodeOf(err) != CodeProtocol || !Retryable(err) {
		t.Errorf("ret -1: %v", err)
	}
}

func TestLongPollEdge524(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.WriteHeader(edgePollTimeoutStatus)
	})

	// Sustained HTTP 524 during GetUpdates (e.g. Tencent EdgeOne ~30s origin timeout on ilinkai.wechat.com)
	// is treated as a normal empty poll and preserves the cursor.
	c := f.client("BT", "https://ilinkai.wechat.com/")
	cur := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	step := 0
	c.now = func() time.Time {
		step++
		if step%2 == 0 {
			return cur.Add(30 * time.Second)
		}
		return cur
	}
	p, err := c.GetUpdates(context.Background(), "C-keep")
	if err != nil {
		t.Fatalf("sustained 524 on GetUpdates should be empty poll, got err: %v", err)
	}
	if p.Cursor != "C-keep" || len(p.Messages) != 0 {
		t.Fatalf("unexpected poll on sustained 524: %+v", p)
	}

	// Sustained HTTP 524 during PollQR is treated as QRWaiting.
	st, err := c.PollQR(context.Background(), "QC", "", "https://ilinkai.wechat.com/", longPollTimeout)
	if err != nil || st.State != QRWaiting {
		t.Fatalf("sustained 524 on PollQR should be QRWaiting, got %+v %v", st, err)
	}

	// Fast HTTP 524 during GetUpdates still returns a retryable CodeHTTP error (avoiding a hot retry loop).
	fast := f.client("BT", "https://ilinkai.wechat.com/")
	fast.now = func() time.Time { return cur }
	if _, err := fast.GetUpdates(context.Background(), "C-keep"); CodeOf(err) != CodeHTTP || !Retryable(err) {
		t.Fatalf("fast 524 on GetUpdates should be retryable HTTP error, got %v", err)
	}

	// HTTP 524 on non-poll endpoints (such as SendText and UploadImage) still returns a retryable CodeHTTP error even if slow.
	if err := c.SendText(context.Background(), "owner@im", "CT", "cid", "hi"); CodeOf(err) != CodeHTTP || !Retryable(err) {
		t.Fatalf("524 on SendText should be retryable HTTP error, got %v", err)
	}
	if _, err := c.UploadImage(context.Background(), "owner@im", []byte("png")); CodeOf(err) != CodeHTTP || !Retryable(err) {
		t.Fatalf("524 on UploadImage should be retryable HTTP error, got %v", err)
	}
}

func TestSendTextParentDeadlineIsTimeout(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		t.Error("an already-expired parent deadline should not reach the server")
		writeJSON(w, map[string]any{"ret": 0})
	})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := f.client("BT", "").SendText(ctx, "owner@im", "CT", "cid", "回答")
	if CodeOf(err) != CodeTimeout || ErrorType(err) != "timeout" {
		t.Fatalf("parent deadline: code=%q type=%q err=%v", CodeOf(err), ErrorType(err), err)
	}
}

func TestSendTextAndTyping(t *testing.T) {
	var got []map[string]any
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		body["path"] = r.URL.Path
		got = append(got, body)
		if r.URL.Path == "/ilink/bot/getconfig" {
			writeJSON(w, map[string]any{"ret": 0, "typing_ticket": "TT"})
			return
		}
		writeJSON(w, map[string]any{"ret": 0})
	})
	c := f.client("BT", "")
	ctx := context.Background()
	if err := c.SendText(ctx, "owner@im", "CT", "cid", "回答"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendText(ctx, "owner@im", "", "cid", "x"); CodeOf(err) != CodeProtocol {
		t.Errorf("no context token: %v", err)
	}
	if err := c.SendText(ctx, "owner@im", "CT", "cid", strings.Repeat("字", MaxText+1)); CodeOf(err) != CodeProtocol {
		t.Errorf("too long: %v", err)
	}
	tk, err := c.TypingTicket(ctx, "owner@im", "CT")
	if err != nil || tk != "TT" {
		t.Fatal(tk, err)
	}
	if err := c.SetTyping(ctx, "owner@im", tk, true); err != nil {
		t.Fatal(err)
	}
	msg := got[0]["msg"].(map[string]any)
	item := msg["item_list"].([]any)[0].(map[string]any)
	if msg["to_user_id"] != "owner@im" || msg["client_id"] != "cid" || msg["context_token"] != "CT" ||
		msg["message_type"] != float64(typeBot) || msg["message_state"] != float64(stateFinish) || item["text_item"].(map[string]any)["text"] != "回答" {
		t.Errorf("send %v", msg)
	}
	if got[2]["path"] != "/ilink/bot/sendtyping" || got[2]["status"] != float64(typingOn) || got[2]["typing_ticket"] != "TT" {
		t.Errorf("typing %v", got[2])
	}
}

// A photo sent as the original (12 MiB here) is downloaded; a picture past MaxPicture is refused once decrypted (its
// ciphertext still fits under the read limit). Only received pictures get the larger limit: the same 12 MiB, or 8 MiB
// and one byte, is refused as a voice message, and the 12 MiB photo is not uploaded as an answer's picture.
func TestDownloadTakesAnOriginalPhoto(t *testing.T) {
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	original := bytes.Repeat([]byte{0xab}, 12<<20)
	plains := map[string][]byte{
		"photo":      original,
		"big":        bytes.Repeat([]byte{0xab}, MaxPicture+1),
		"voice-over": bytes.Repeat([]byte{0xab}, 8<<20+1),
	}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		plain, ok := plains[r.URL.Query().Get("encrypted_query_param")]
		if r.URL.Path != "/c2c/download" || !ok {
			t.Errorf("unexpected request %s", r.URL)
			return
		}
		enc, err := EncryptECB(plain, key)
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(enc)
	})
	c := f.client("BT", "")
	ctx, keyHex := context.Background(), hex.EncodeToString(key)
	got, err := c.Download(ctx, Media{Kind: MediaImage, Query: "photo", KeyHex: keyHex})
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("a 12 MiB original: %d bytes, %v", len(got), err)
	}
	if _, err := c.Download(ctx, Media{Kind: MediaImage, Query: "big", KeyHex: keyHex}); !errors.Is(err, ErrMediaTooLarge) {
		t.Fatalf("a picture past MaxPicture: %v", err)
	}
	for _, q := range []string{"photo", "voice-over"} {
		if _, err := c.Download(ctx, Media{Kind: MediaVoice, Query: q, KeyHex: keyHex}); !errors.Is(err, ErrMediaTooLarge) {
			t.Fatalf("voice %s past 8 MiB: %v", q, err)
		}
	}
	if _, err := c.UploadImage(ctx, "peer@im.wechat", original); CodeOf(err) != CodeProtocol {
		t.Fatalf("uploading the 12 MiB photo: %v", err)
	}
}

func TestDownload(t *testing.T) {
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	plain := []byte("picture bytes, not a multiple of sixteen")
	enc, err := EncryptECB(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.URL.Path != "/c2c/download" || r.URL.Query().Get("encrypted_query_param") != "Q" {
			t.Errorf("download %s", r.URL)
		}
		_, _ = w.Write(enc)
	})
	c := f.client("BT", "")
	for _, m := range []Media{
		{Kind: MediaFile, Query: "Q", KeyHex: hex.EncodeToString(key)},
		{Kind: MediaVideo, Query: "Q", KeyB64: base64.StdEncoding.EncodeToString(key)},
		{Kind: MediaFile, Query: "Q", KeyB64: base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(key)))},
		{Kind: MediaImage, URL: "https://novac2c.cdn.wechat.com/c2c/download?encrypted_query_param=Q", KeyHex: hex.EncodeToString(key)},
		{Kind: MediaVoice, URL: "https://novac2c.cdn.wechat.com/c2c/download?encrypted_query_param=Q", KeyB64: base64.StdEncoding.EncodeToString(key)},
		{Kind: MediaFile, URL: "https://novac2c.cdn.wechat.com/c2c/download?encrypted_query_param=Q", KeyB64: base64.StdEncoding.EncodeToString(key)},
		{Kind: MediaVideo, URL: "https://wechat.com/c2c/download?encrypted_query_param=Q", KeyB64: base64.StdEncoding.EncodeToString(key)},
	} {
		got, err := c.Download(context.Background(), m)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%+v: %q %v", m, got, err)
		}
	}
	if len(f.hosts) != 7 || f.hosts[0] != "novac2c.cdn.weixin.qq.com" ||
		f.hosts[3] != "novac2c.cdn.wechat.com" || f.hosts[4] != "novac2c.cdn.wechat.com" || f.hosts[5] != "novac2c.cdn.wechat.com" || f.hosts[6] != "wechat.com" {
		t.Errorf("host %v", f.hosts)
	}
	for _, kind := range []MediaKind{MediaImage, MediaVoice, MediaFile, MediaVideo} {
		for _, bad := range []string{
			"https://evil.example/x",
			"https://evilwechat.com/c2c/download?encrypted_query_param=MEDIA_SECRET",
			"https://wechat.com.evil.com/x",
			"http://novac2c.cdn.wechat.com/x",
			"https://novac2c.cdn.wechat.com:8443/x",
			(&url.URL{
				Scheme:   "https",
				User:     url.UserPassword("u", "media_pass"),
				Host:     "novac2c.cdn.wechat.com",
				Path:     "/secret_path",
				RawQuery: "encrypted_query_param=MEDIA_SECRET",
			}).String(),
		} {
			_, err := c.Download(context.Background(), Media{Kind: kind, URL: bad, KeyHex: hex.EncodeToString(key)})
			if CodeOf(err) != CodeProtocol {
				t.Errorf("bad %s media URL %s: %v", kind, bad, err)
				continue
			}
			if !strings.Contains(err.Error(), "source=media_url") ||
				strings.Contains(err.Error(), "MEDIA_SECRET") || strings.Contains(err.Error(), "media_pass") || strings.Contains(err.Error(), "secret_path") {
				t.Errorf("media URL diagnostic leaked secret for %s %s: %v", kind, bad, err)
			}
		}
	}
	if _, err := DecryptECB(enc[:16], key); err == nil {
		t.Error("bad padding accepted")
	}
}

func TestDownloadFileAndVideoSizeLimits(t *testing.T) {
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	tests := []struct {
		name, query string
		kind        MediaKind
		size        int
		ok          bool
	}{
		{name: "file at the message budget", query: "at", kind: MediaFile, size: 40 << 20, ok: true},
		{name: "one byte over", query: "over", kind: MediaFile, size: 40<<20 + 1},
		{name: "video over the picture limit", query: "video", kind: MediaVideo, size: 9 << 20, ok: true},
	}
	downloads := map[string][]byte{}
	for _, tt := range tests {
		enc, err := EncryptECB(make([]byte, tt.size), key)
		if err != nil {
			t.Fatal(err)
		}
		downloads[tt.query] = enc
	}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		_, _ = w.Write(downloads[r.URL.Query().Get("encrypted_query_param")])
	})
	c := f.client("BT", "")
	for _, tt := range tests {
		got, err := c.Download(context.Background(), Media{Kind: tt.kind, Query: tt.query, KeyB64: base64.StdEncoding.EncodeToString(key)})
		if tt.ok && (err != nil || len(got) != tt.size) {
			t.Errorf("%s: decrypted %d bytes, want %d; error=%v", tt.name, len(got), tt.size, err)
		}
		if !tt.ok && (err == nil || CodeOf(err) != CodeBadResponse || got != nil) {
			t.Errorf("%s: decrypted %d bytes, error=%v; want media-too-large rejection", tt.name, len(got), err)
		}
	}
}

func TestFilterMarkdown(t *testing.T) {
	in := "##### 小标题\n**粗体** *斜体* *italic* ~~删除~~ ![图](https://x/y.png) `*代码*`\n```\n~~原样~~ *斜*\n```\n"
	want := "小标题\n**粗体** 斜体 *italic* 删除  `*代码*`\n```\n~~原样~~ *斜*\n```\n"
	if got := FilterMarkdown(in); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestChunk(t *testing.T) {
	if Chunk("", 0) != nil {
		t.Error("empty text has chunks")
	}
	text := strings.Repeat("一二三四五 ", 30) + "\n```\n" + strings.Repeat("code line\n", 30) + "```\nend"
	chunks := Chunk(text, 100)
	if len(chunks) < 3 {
		t.Fatalf("%d chunks", len(chunks))
	}
	var joined []string
	for i, c := range chunks {
		if n := len([]rune(c)); n > 100 {
			t.Errorf("chunk %d has %d code points", i, n)
		}
		if fences(c)%2 != 0 {
			t.Errorf("chunk %d leaves a fence open: %q", i, c)
		}
		joined = append(joined, c)
	}
	if !strings.Contains(strings.Join(joined, ""), "end") {
		t.Error("lost the end")
	}
}

// getuploadurl's full upload URL is taken only on a Tencent HTTPS origin; otherwise the picture goes to the CDN's
// upload with the encrypted parameter, and with neither there is nowhere to upload.
func TestUploadURL(t *testing.T) {
	for _, ok := range []string{
		"https://upload.weixin.qq.com/c2c/upload?x=1",
		"https://upload.wechat.com/c2c/upload?x=1",
		"https://novac2c.cdn.wechat.com/c2c/upload?x=1",
		"https://wechat.com/c2c/upload?x=1",
	} {
		if got, err := uploadURL(ok, "", "FK"); err != nil || got != ok {
			t.Errorf("Tencent full URL %s: %q %v", ok, got, err)
		}
	}
	for _, bad := range []string{
		"http://upload.weixin.qq.com/x",
		"http://upload.wechat.com/x",
		"https://evil.example/x",
		"https://evilwechat.com/c2c/upload?encrypted_query_param=UPLOAD_SECRET",
		"https://wechat.com.evil.com/x",
		(&url.URL{Scheme: "https", User: url.UserPassword("u", "p"), Host: "upload.weixin.qq.com", Path: "/x"}).String(),
		(&url.URL{
			Scheme:   "https",
			User:     url.UserPassword("u", "upload_pass"),
			Host:     "upload.wechat.com",
			Path:     "/secret_upload_path",
			RawQuery: "encrypted_query_param=UPLOAD_SECRET",
		}).String(),
		"https://upload.weixin.qq.com:8443/x",
		"https://upload.wechat.com:8443/x",
	} {
		_, err := uploadURL(bad, "P", "FK")
		if CodeOf(err) != CodeProtocol {
			t.Errorf("%s: %v", bad, err)
			continue
		}
		if !strings.Contains(err.Error(), "source=upload_url") ||
			strings.Contains(err.Error(), "UPLOAD_SECRET") || strings.Contains(err.Error(), "upload_pass") || strings.Contains(err.Error(), "secret_upload_path") {
			t.Errorf("upload URL diagnostic leaked secret for %s: %v", bad, err)
		}
	}
	if got, err := uploadURL("", "a+b/c", "FK"); err != nil || got != "https://novac2c.cdn.weixin.qq.com/c2c/upload?encrypted_query_param=a%2Bb%2Fc&filekey=FK" {
		t.Errorf("param: %q %v", got, err)
	}
	if _, err := uploadURL("", "", "FK"); CodeOf(err) != CodeBadResponse {
		t.Errorf("neither: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUploadAndSendImageInternationalWeChat(t *testing.T) {
	var hosts []string
	var uploadedCipher []byte
	var sentImageItem map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getuploadurl":
			writeJSON(w, map[string]any{
				"ret":             0,
				"upload_full_url": "https://novac2c.cdn.wechat.com/c2c/upload?encrypted_query_param=UP-INTL&filekey=FK",
			})
		case "/c2c/upload":
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upload body: %v", err)
			}
			uploadedCipher = b
			w.Header().Set("x-encrypted-param", "DL-INTL")
			w.WriteHeader(http.StatusOK)
		case "/ilink/bot/sendmessage":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode sendmessage: %v", err)
			}
			msg := body["msg"].(map[string]any)
			sentImageItem = msg["item_list"].([]any)[0].(map[string]any)["image_item"].(map[string]any)
			writeJSON(w, map[string]any{"ret": 0, "message_id": 42})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	srvURL, _ := url.Parse(srv.URL)
	c, err := New(Options{
		BaseURL: "https://ilinkai.wechat.com/",
		Token:   "BT-INTL",
		Agent:   "Persona/1",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			hosts = append(hosts, r.URL.Host)
			r2 := r.Clone(r.Context())
			r2.URL.Scheme, r2.URL.Host, r2.Host = srvURL.Scheme, srvURL.Host, srvURL.Host
			return http.DefaultTransport.RoundTrip(r2)
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("\x89PNG\r\n\x1a\ninternational wechat picture")
	up, err := c.UploadImage(context.Background(), "owner@im", plain)
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	if up.Query != "DL-INTL" || up.CipherSize != len(uploadedCipher) {
		t.Fatalf("uploaded %+v, cipher len %d", up, len(uploadedCipher))
	}
	key, err := hex.DecodeString(up.KeyHex)
	if err != nil {
		t.Fatalf("key hex %q: %v", up.KeyHex, err)
	}
	gotPlain, err := DecryptECB(uploadedCipher, key)
	if err != nil || !bytes.Equal(gotPlain, plain) {
		t.Fatalf("decrypted %q %v", gotPlain, err)
	}
	id, err := c.SendImage(context.Background(), "owner@im", "CT", "cid-img", up)
	if err != nil || id != "42" {
		t.Fatalf("SendImage = %q, %v", id, err)
	}
	if len(hosts) != 3 || hosts[0] != "ilinkai.wechat.com" || hosts[1] != "novac2c.cdn.wechat.com" || hosts[2] != "ilinkai.wechat.com" {
		t.Fatalf("hosts = %v", hosts)
	}
	if sentImageItem == nil || sentImageItem["media"].(map[string]any)["encrypt_query_param"] != "DL-INTL" {
		t.Fatalf("sent image_item = %v", sentImageItem)
	}
}
