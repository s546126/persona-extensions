package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPullSplitsByModelAndLauncher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		model, launcher := q.Get("model"), q.Get("launcher")
		switch {
		case model == "" && launcher == "":
			write(w, map[string]any{"usage": map[string]any{
				"models":    []Share{{Name: "alpha", In: 100}, {Name: "beta", In: 40}},
				"launchers": []Share{{Name: "cli", In: 100}, {Name: "-", In: 40}},
			}})
		case model == "alpha" && launcher == "cli":
			write(w, series(600, 60, 100, 0))
		case model == "alpha" && launcher == "-":
			write(w, series(600, 60, 0, 0))
		case model == "beta" && launcher == "cli":
			write(w, series(600, 60, 0, 0))
		case model == "beta" && launcher == "-":
			write(w, series(600, 600, 40, 0))
		default:
			http.Error(w, "unexpected "+r.URL.RawQuery, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	got, err := New(srv.URL, "tok", nil).Pull(context.Background(), "m-a", "1h")
	if err != nil {
		t.Fatal(err)
	}
	// Empty pairs produce zero correction tombstones. beta's point covers 10 minutes, so it becomes 10 buckets summing to 40.
	sums := map[string]int64{}
	for _, b := range got {
		sums[b.Model+"|"+b.Launcher] += b.Total()
	}
	if sums["alpha|cli"] != 100 || sums["beta|"] != 40 || sums["alpha|"] != 0 || sums["beta|cli"] != 0 || len(sums) != 4 {
		t.Fatalf("sums %v, want alpha|cli 100, beta| 40, and both zero-valued empty pairs", sums)
	}
}

func TestBucketsSpreadAPointOverItsMinutes(t *testing.T) {
	s := Series{Step: 600, Points: []Point{{T: 1000, Step: 600, In: 605, Out: 3, Req: 2}}}
	got := bucketsOf(s, "alpha", "cli", true)
	if len(got) != 10 {
		t.Fatalf("got %d minutes, want 10", len(got))
	}
	var in, out int64
	var req int
	for i, b := range got {
		in += b.Input
		out += b.Output
		req += b.Requests
		if b.Minute != int64(1000+i*60) || b.Model != "alpha" || b.Launcher != "cli" {
			t.Errorf("minute %d: %+v", i, b)
		}
	}
	if in != 605 || out != 3 || req != 2 {
		t.Fatalf("minutes sum to in %d out %d req %d, want 605, 3, 2", in, out, req)
	}
	if got[0].Input != 65 { // 605 = 65 + 9*60
		t.Fatalf("first minute keeps the remainder: %d, want 65", got[0].Input)
	}
}

func TestGatewayAccountsHiddenWhenUnavailable(t *testing.T) {
	c := New("http://example.invalid", "tok", func(ctx context.Context, method string, out any) error {
		return errNope
	})
	if _, ok := c.GatewayAccounts(context.Background()); ok {
		t.Fatal("a failing method was shown")
	}
	ok := New("http://example.invalid", "tok", func(ctx context.Context, method string, out any) error {
		*(out.(*GatewayAccounts)) = GatewayAccounts{Accounts: []GatewayAccount{{Name: "one"}}}
		return nil
	})
	got, shown := ok.GatewayAccounts(context.Background())
	if !shown || len(got.Accounts) != 1 {
		t.Fatalf("shown %v accounts %+v", shown, got.Accounts)
	}
}

var errNope = &nopError{}

type nopError struct{}

func (nopError) Error() string { return "nope" }

func series(t, step int64, in, out int64) map[string]any {
	return map[string]any{"usage": Series{Step: int(step), Points: []Point{{T: t, In: in, Out: out}}}}
}

func write(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

// A hub error body is kept for the log cut to 200 bytes, and the cut never leaves half a character: the 200th byte is inside the
// 67th Han character, and inside the 50th emoji after one ASCII byte.
func TestAnErrorBodyIsCutAtACharacterBoundary(t *testing.T) {
	for _, tt := range []struct{ name, body, want string }{
		{"han", strings.Repeat("汉", 100), strings.Repeat("汉", 66)},
		{"emoji", "a" + strings.Repeat("😀", 60), "a" + strings.Repeat("😀", 49)},
		{"ascii", strings.Repeat("x", 300), strings.Repeat("x", 200)},
		{"short han", strings.Repeat("汉", 66), strings.Repeat("汉", 66)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tt.body, http.StatusBadGateway)
			}))
			defer srv.Close()

			_, err := New(srv.URL, "tok", nil).Machines(context.Background())
			if err == nil {
				t.Fatal("a 502 was not an error")
			}
			var he *Error
			if !errors.As(err, &he) {
				t.Fatalf("error %T, want *Error", err)
			}
			want := "HTTP 502：" + tt.want
			if he.Detail != want || !utf8.ValidString(he.Detail) {
				t.Fatalf("detail %q (valid UTF-8 %v), want %q", he.Detail, utf8.ValidString(he.Detail), want)
			}
		})
	}
}

// What the user reads for a failed call says what happened and what to do, in plain Chinese: no "hub", no "HTTP" and no
// raw body; the technical detail stays in Detail for the log.
func TestErrorsTellTheCauseAndWhatToDo(t *testing.T) {
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token expired", http.StatusUnauthorized)
	}))
	defer refused.Close()
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer busy.Close()
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	for _, tt := range []struct {
		name string
		c    *Client
		want string
	}{
		{"no token", New("", "", nil), "关掉再打开"},
		{"refused", New(refused.URL, "tok", nil), "关掉再打开"},
		{"busy", New(busy.URL, "tok", nil), "自动再试"},
		{"too old", New(old.URL, "tok", nil), "更新到最新版"},
		{"unreachable", New(gone.URL, "tok", nil), "暂时连不上 Persona"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.c.Machines(context.Background())
			if err == nil {
				t.Fatal("no error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "Persona") || !strings.Contains(msg, tt.want) || strings.Contains(msg, "关机") {
				t.Fatalf("message %q lacks the cause or %q", msg, tt.want)
			}
			for _, bad := range []string{"hub", "HTTP", "token expired", "upstream", "127.0.0.1", "dial"} {
				if strings.Contains(msg, bad) {
					t.Fatalf("message %q shows %q", msg, bad)
				}
			}
			if LogText(err) == msg && tt.name != "no token" {
				t.Fatalf("the log text has no detail: %q", LogText(err))
			}
		})
	}
}

// An answer that is not what was asked for, a 200 with an HTML page or a body cut short, is an *Error in the user's
// words; Go's English error stays in Detail.
func TestABadAnswerIsToldInPlainWords(t *testing.T) {
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>sign in</body></html>")
	}))
	defer html.Close()
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, `{"machines": [`)
	}))
	defer cut.Close()
	half := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"machines": [{"id": "m-a"`)
	}))
	defer half.Close()
	for _, tt := range []struct {
		name, base, want, detail string
	}{
		{"html", html.URL, "读不懂", "invalid character"},
		{"cut short", cut.URL, "中途断了", "unexpected EOF"},
		{"half a JSON", half.URL, "读不懂", "unexpected end"},
		{"bad address", "http://bad host", "地址用不了", "invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.base, "tok", nil).Machines(context.Background())
			var he *Error
			if !errors.As(err, &he) {
				t.Fatalf("error %T %v, want *Error", err, err)
			}
			if !strings.Contains(he.Message, tt.want) || strings.ContainsAny(he.Message, "<>{}") || strings.Contains(he.Message, "invalid") {
				t.Fatalf("message %q", he.Message)
			}
			if !strings.Contains(he.Detail, tt.detail) {
				t.Fatalf("detail %q lacks %q", he.Detail, tt.detail)
			}
		})
	}
}
