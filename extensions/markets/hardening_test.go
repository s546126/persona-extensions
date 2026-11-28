package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/wire"
)

func checkPluginkitView(t *testing.T, v pluginkit.View) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	if err := wire.CheckViewJSON(raw); err != nil {
		t.Fatalf("wire.CheckViewJSON failed: %v", err)
	}
}

func TestHardeningConcurrentConfigureAndReadsRace(t *testing.T) {
	fx := newFakeOKX(t)
	p := newPlugin(func(string, ...any) {})
	p.srcs = sources{newOKX(fx.srv.URL)}
	dir := t.TempDir()
	initParams := func(colors, tz, bar string) pluginkit.InitParams {
		cfg, _ := json.Marshal(map[string]any{
			"colors":        colors,
			"tz":            tz,
			"bar":           bar,
			"overview_card": true,
			"watchlist":     []string{"BTC-USDT"},
		})
		ip := pluginkit.InitParams{Config: cfg}
		ip.Plugin.DataDir = dir
		return ip
	}
	if err := p.configure(initParams("red_up", "UTC", "1H")); err != nil {
		t.Fatalf("initial configure: %v", err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		for i := range 40 {
			if i%2 == 0 {
				_ = p.configure(initParams("green_up", "Asia/Shanghai", "4H"))
			} else {
				_ = p.configure(initParams("red_up", "UTC", "1H"))
			}
		}
	}()
	go func() {
		defer wg.Done()
		req, _ := json.Marshal(pluginkit.UIRequest{Contribution: "watchlist", UIVersion: 3})
		for range 40 {
			_, _ = p.uiRender(ctx, nil, req)
		}
	}()
	go func() {
		defer wg.Done()
		for range 40 {
			_, _ = p.card(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		for range 40 {
			_, _ = p.cInboxCollect(ctx, nil, nil)
			_, _ = p.toolCandles(ctx, "BTC-USDT", "", 5)
		}
	}()
	wg.Wait()
}

func TestHardeningAlertRejectsNaNAndPreservesDisk(t *testing.T) {
	fx := newFakeOKX(t)
	p := newPlugin(func(string, ...any) {})
	p.srcs = sources{newOKX(fx.srv.URL)}
	dir := t.TempDir()
	ip := pluginkit.InitParams{}
	ip.Plugin.DataDir = dir
	if err := p.configure(ip); err != nil {
		t.Fatalf("configure: %v", err)
	}

	// First create a valid alert and ensure it is saved to disk.
	ref := Ref{Source: "okx", Symbol: "BTC-USDT"}
	good, err := p.newAlert(context.Background(), ref, "above", "60000")
	if err != nil {
		t.Fatalf("valid newAlert failed: %v", err)
	}
	if err := p.alerts.add(good); err != nil {
		t.Fatalf("add good alert: %v", err)
	}
	alertsPath := filepath.Join(dir, "alerts.json")
	before, err := os.ReadFile(alertsPath)
	if err != nil || len(before) == 0 {
		t.Fatalf("expected saved alerts.json before NaN attempt, err=%v len=%d", err, len(before))
	}

	// NaN must be rejected and must not corrupt or wipe alerts.json.
	for _, bad := range []string{"NaN", "nan", "-NaN", "Inf", "-Inf"} {
		if _, err := p.newAlert(context.Background(), ref, "above", bad); err == nil {
			t.Fatalf("expected newAlert to reject %q, but it succeeded", bad)
		}
	}

	// Also verify saveLocked refuses non-finite Alert without overwriting disk.
	if err := p.alerts.add(Alert{ID: "badnan", Ref: ref.String(), Kind: "above", Value: math.NaN()}); err == nil {
		t.Fatalf("expected alerts.add with NaN value to fail")
	}
	after, err := os.ReadFile(alertsPath)
	if err != nil {
		t.Fatalf("read alerts.json after bad attempt: %v", err)
	}
	if len(after) == 0 {
		t.Fatalf("alerts.json was wiped to 0 bytes!")
	}
	var saved []Alert
	if err := json.Unmarshal(after, &saved); err != nil || len(saved) != 1 || saved[0].ID != good.ID {
		t.Fatalf("alerts.json corrupted: err=%v saved=%+v", err, saved)
	}
}

func TestHardeningOversizedHTTPResponsesRejected(t *testing.T) {
	t.Run("tencent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			// Write 4 MiB + 128 bytes of syntactically valid quote-like lines.
			chunk := strings.Repeat("v_sh600519=\"1~Moutai~600519~1500.00\";\n", 1024)
			for written := 0; written <= (4<<20)+128; written += len(chunk) {
				_, _ = w.Write([]byte(chunk))
			}
		}))
		defer srv.Close()
		tc := newTencent(tencentHosts{quote: srv.URL, search: srv.URL, kline: srv.URL, mkline: srv.URL})
		if _, err := tc.fetch(context.Background(), srv.URL+"/q=sh600519"); err == nil {
			t.Fatalf("expected tencent.fetch to reject oversized (>4 MiB) response instead of silently truncating")
		}
	})

	t.Run("okx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":"0","msg":"","data":"`))
			chunk := strings.Repeat("a", 64<<10)
			for written := 0; written <= (8<<20)+128; written += len(chunk) {
				_, _ = w.Write([]byte(chunk))
			}
			_, _ = w.Write([]byte(`"}`))
		}))
		defer srv.Close()
		o := newOKX(srv.URL)
		_, err := o.fetch(context.Background(), "/api/v5/market/tickers?instType=SPOT")
		if err == nil {
			t.Fatalf("expected okx.fetch to reject oversized response")
		}
		if !strings.Contains(err.Error(), "太大") && !strings.Contains(strings.ToLower(err.Error()), "too large") {
			t.Fatalf("expected explicit oversized error, got: %v", err)
		}
	})
}

func TestHardeningKlineCapsAndNonFiniteSanitized(t *testing.T) {
	p := newPlugin(func(string, ...any) {})
	q := Quote{Symbol: "BTC-USDT", Last: 50000, Base: 49000, Currency: "USDT", Period: "24h"}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cs := make([]Candle, 0, 150)
	for i := range 150 {
		c := Candle{At: start.Add(time.Duration(i) * time.Hour), Open: 100, High: 110, Low: 90, Close: 105, Vol: 10}
		if i == 10 {
			c.Close = math.NaN()
			c.Vol = math.Inf(1)
		}
		cs = append(cs, c)
	}
	items := klineItems(cs, "1H", time.UTC, true, q, p.tone)
	checkPluginkitView(t, pluginkit.View{
		Title:    "BTC-USDT",
		Sections: []pluginkit.Section{{Title: "K 线", Items: items}},
	})
}

func TestHardeningOKXNonFiniteTickerValues(t *testing.T) {
	tk := okxTicker{
		InstID:    "BTC-USDT",
		InstType:  "SPOT",
		Last:      "NaN",
		Open24h:   "Inf",
		High24h:   "NaN",
		Low24h:    "-Inf",
		VolCcy24h: "NaN",
	}
	q := tk.quote()
	if math.IsNaN(q.Last) || math.IsInf(q.Last, 0) ||
		math.IsNaN(q.Base) || math.IsInf(q.Base, 0) ||
		math.IsNaN(q.High) || math.IsInf(q.High, 0) ||
		math.IsNaN(q.Low) || math.IsInf(q.Low, 0) ||
		math.IsNaN(q.Turnover) || math.IsInf(q.Turnover, 0) {
		t.Fatalf("expected non-finite ticker values to be sanitized to 0, got %+v", q)
	}
}

func TestHardeningTencentUSCodeMapBounded(t *testing.T) {
	tc := newTencent(tencentBase)
	m := &txMarket{t: tc, m: mUS}
	fields := make([]string, 60)
	fields[1] = "Test Co"
	fields[3] = "100.5"
	fields[4] = "99.0"
	for i := range 1000 {
		sym := fmt.Sprintf("SYM%d", i)
		fields[2] = sym + ".OQ"
		m.parseQuote(sym, fields)
	}
	tc.mu.Lock()
	n := len(tc.usCode)
	tc.mu.Unlock()
	if n > 500 {
		t.Fatalf("tencent.usCode grew unbounded: len=%d", n)
	}
}

func TestHardeningClipNonPositiveNoPanic(t *testing.T) {
	if got := clip("hello", 0); got != "" {
		t.Fatalf("clip(hello, 0) = %q, want empty", got)
	}
	if got := clip("hello", -5); got != "" {
		t.Fatalf("clip(hello, -5) = %q, want empty", got)
	}
	if got := clip("hello", 1); got != "…" {
		t.Fatalf("clip(hello, 1) = %q, want ellipsis", got)
	}
}
