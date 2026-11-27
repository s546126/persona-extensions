package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

const testQuotas = `{"data":[
	{"kind":"subscription","provider":"claude","alias":"Studio Primary","user":"one@example.invalid","name":"Claude Code","plan":"max",
	 "readAt":"2026-11-25T18:05:45.123456789+08:00","windows":[
	 {"name":"5 hours","used":0,"holds":{"cost":12345,"tokens":123456789}},
	 {"name":"7 days","used":50,"resetsAt":"2026-11-26T19:00:00+08:00"},
	 {"name":"7 days · Fable","used":100,"resetsAt":"2026-11-26T19:00:00+08:00"},
	 {"name":"7 days","used":null},{"name":"5 hours"},{"name":"5 hours","used":0,"text":true},
	 {"name":"7 days","used":0,"unlimited":true}]},
	{"kind":"subscription","provider":"claude","user":"two@example.invalid","name":"Claude Code","plan":"max 20x",
	 "asOf":"2026-11-24T08:00:00Z","readError":"upstream-timeout","windows":[{"name":"7 days","used":60,"resetsAt":"bad"}]},
	{"kind":"subscription","provider":"codex","user":"three@example.invalid","name":"Codex","plan":"promax",
	 "windows":[{"name":"7 days","used":50},{"name":"Image generation","used":0,"text":true}]},
	{"kind":"subscription","provider":"agy","user":"four@example.invalid","name":"Antigravity","plan":"pro",
	 "readAt":"2026-11-25T10:00:00Z","windows":[
	 {"name":"Gemini · 7 days","pool":"Gemini","used":40,"resetsAt":"2026-11-29T00:00:00Z"},
	 {"name":"Claude & GPT · 5 hours","pool":"Claude & GPT","used":15},
	 {"name":"Gemini · 5 hours","pool":"Gemini","used":25},
	 {"name":"Claude & GPT · 7 days","pool":"Claude & GPT","used":55}]},
	{"kind":"subscription","provider":"Zhipu","name":"Zhipu Seat","plan":"vip",
	 "readAt":"2026-11-25T10:00:00Z","windows":[
	 {"name":"Daily","used":12},
	 {"name":"MCP · Month","used":33},
	 {"name":"Wallet","used":0,"balance":500}]},
	{"kind":"subscription","provider":"relay","user":"relay@example.invalid","plan":"已登录","windows":[]},
	{"kind":"subscription","provider":"empty-text","user":"textonly@example.invalid","readAt":"2026-11-25T10:00:00Z",
	 "windows":[{"name":"Credits","used":10},{"name":"Extra usage","used":100,"text":true}]},
	{"kind":"plan","provider":"claude","user":"plan@example.invalid","balance":"99999","windows":[{"name":"Monthly","used":20}]},
	{"kind":"subscription","provider":"claude","windows":[]}
]}`

func TestProtocol(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	exttest.Check(t, manifest, exttest.Serve(t, program()))
	t.Setenv("PERSONA_EXTENSION_DATA", t.TempDir())
	exttest.StartWith(t, manifest, exttest.Serve(t, program()), func(p *wire.InitializeParams) {
		p.Extension, p.Plugin = nil, nil
	})
}

func TestUsageQueryDefaultNamesAndAllProviders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" || req.URL.Path != quotaPath || req.URL.RawQuery != "kept=1" ||
			req.Header.Get("Authorization") != "" {
			t.Error("the local request must read only quotas, without a key")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(testQuotas))
	}))
	defer srv.Close()
	h := exttest.StartWith(t, manifest, exttest.Serve(t, program()), func(init *wire.InitializeParams) {
		init.Config, _ = json.Marshal(map[string]any{"local_address": srv.URL})
		init.Secrets = map[string]string{"magpie_quota_key": "fixture"}
	})
	var raw json.RawMessage
	if err := h.Call("usage.query", map[string]string{"contribution": "magpie", "lang": "en"}, &raw); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"cost"`, `"tokens"`, `"balance"`, `"holds"`, `"remaining"`, `"upstream-timeout"`, `"relay@example.invalid"`, `"plan@example.invalid"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("unexpected non-quota or excluded field appeared: %s", forbidden)
		}
	}
	var result struct{ Accounts []account }
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Accounts) != 5 {
		t.Fatalf("expected 5 accounts (antigravity, 2 claude, codex, zhipu), got %d: %+v (%v)", len(result.Accounts), result.Accounts, err)
	}
	byName := make(map[string]account, len(result.Accounts))
	for _, row := range result.Accounts {
		byName[row.Name] = row
	}

	// Alias wins over user; plan is title-cased; 5h / 7d / 7d·Fable parsed generically.
	primary, ok := byName["Studio Primary"]
	if !ok {
		t.Fatalf("missing alias-named account: %+v", result.Accounts)
	}
	if primary.Provider != "claude" || primary.Plan != "Max" || primary.SampledAt == nil ||
		primary.SampledAt.Format("2006-01-02T15:04:05Z") != "2026-11-25T10:05:45Z" {
		t.Fatalf("primary: %+v", primary)
	}
	if len(primary.Windows) != 3 {
		t.Fatalf("primary windows: %+v", primary.Windows)
	}
	if primary.Windows[0].Label != "5h" || primary.Windows[0].WindowMinutes != 300 || primary.Windows[0].Used != 0 || primary.Windows[0].Model != "" ||
		primary.Windows[1].Label != "7d" || primary.Windows[1].WindowMinutes != 10080 || primary.Windows[1].Used != 50 || primary.Windows[1].ResetsAt == nil ||
		primary.Windows[2].Label != "7d" || primary.Windows[2].WindowMinutes != 10080 || primary.Windows[2].Model != "Fable" || primary.Windows[2].Title != "Fable" || primary.Windows[2].Used != 100 {
		t.Fatalf("primary windows mismatch: %+v", primary.Windows)
	}

	// Empty alias falls back to user; multiplier plan normalized; cached error shows localized notice.
	second, ok := byName["two@example.invalid"]
	if !ok {
		t.Fatalf("missing user-named account: %+v", result.Accounts)
	}
	if second.ID == primary.ID || second.Plan != "Max 20x" || len(second.Windows) != 1 || second.Notice == "" ||
		second.SampledAt == nil || second.SampledAt.Day() != 24 {
		t.Fatalf("second account: %+v", second)
	}

	// Undated subscription with periodic window keeps freshness guard and normalized Promax plan.
	codex, ok := byName["three@example.invalid"]
	if !ok {
		t.Fatalf("missing codex account: %+v", result.Accounts)
	}
	if codex.Provider != "codex" || codex.Plan != "Promax" || len(codex.Windows) != 0 || codex.SampledAt != nil || codex.Notice == "" {
		t.Fatalf("codex account: %+v", codex)
	}

	// Canonicalized provider "agy" -> "antigravity" with contiguous pool groups sorted shortest-first.
	agy, ok := byName["four@example.invalid"]
	if !ok {
		t.Fatalf("missing antigravity account: %+v", result.Accounts)
	}
	if agy.Provider != "antigravity" || agy.Plan != "Pro" || len(agy.Windows) != 4 {
		t.Fatalf("antigravity account: %+v", agy)
	}
	wantAgy := []struct {
		label, model, group, title string
		mins                       int
	}{
		{"5h", "Gemini", "Gemini", "Gemini", 300},
		{"7d", "Gemini", "Gemini", "Gemini", 10080},
		{"5h", "Claude & GPT", "Claude & GPT", "Claude & GPT", 300},
		{"7d", "Claude & GPT", "Claude & GPT", "Claude & GPT", 10080},
	}
	for i, want := range wantAgy {
		got := agy.Windows[i]
		if got.Label != want.label || got.WindowMinutes != want.mins || got.Model != want.model || got.Group != want.group || got.Title != want.title {
			t.Fatalf("antigravity window %d: got %+v, want %+v", i, got, want)
		}
	}

	// Empty alias and user falls back to name; Daily + MCP·Month parsed; wallet balance window skipped.
	zhipu, ok := byName["Zhipu Seat"]
	if !ok {
		t.Fatalf("missing name-fallback account: %+v", result.Accounts)
	}
	if zhipu.Provider != "zhipu" || zhipu.Plan != "VIP" || len(zhipu.Windows) != 2 {
		t.Fatalf("zhipu account: %+v", zhipu)
	}
	if zhipu.Windows[0].Label != "1d" || zhipu.Windows[0].WindowMinutes != 1440 || zhipu.Windows[0].Model != "" ||
		zhipu.Windows[1].Label != "30d" || zhipu.Windows[1].WindowMinutes != 43200 || zhipu.Windows[1].Model != "MCP" || zhipu.Windows[1].Title != "MCP" {
		t.Fatalf("zhipu windows: %+v", zhipu.Windows)
	}
}

func TestProviderFiltersAndAnonymousMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testQuotas))
	}))
	defer srv.Close()

	// Exclude codex and enable anonymous display mode.
	h := exttest.StartWith(t, manifest, exttest.Serve(t, program()), func(init *wire.InitializeParams) {
		init.Config, _ = json.Marshal(map[string]any{
			"local_address":     srv.URL,
			"exclude_providers": []string{"codex", "agy"},
			"anonymous_names":   true,
		})
	})
	var raw json.RawMessage
	if err := h.Call("usage.query", map[string]string{"contribution": "magpie", "lang": "en"}, &raw); err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{"@example.invalid", "Studio Primary", "Zhipu Seat"} {
		if strings.Contains(string(raw), sensitive) {
			t.Fatalf("anonymous mode leaked %q: %s", sensitive, raw)
		}
	}
	var res struct{ Accounts []account }
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Accounts) != 3 {
		t.Fatalf("expected 3 accounts after excluding codex & agy, got %d: %+v", len(res.Accounts), res.Accounts)
	}
	for _, row := range res.Accounts {
		if row.Provider == "codex" || row.Provider == "antigravity" {
			t.Fatalf("excluded provider still present: %+v", row)
		}
		if !strings.HasPrefix(row.Name, "Magpie ") {
			t.Fatalf("expected opaque alias in anonymous mode, got %q", row.Name)
		}
	}

	// Include only claude + zhipu, then also exclude zhipu (exclude wins).
	opts := buildMapOptions([]string{"Claude", "Zhipu"}, []string{"zhipu"}, false)
	filtered, err := mapQuotas(testQuotas, make([]byte, 32), "scope", "en", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 {
		t.Fatalf("expected 2 claude accounts, got %d: %+v", len(filtered), filtered)
	}
	for _, row := range filtered {
		if row.Provider != "claude" {
			t.Fatalf("unexpected provider %q", row.Provider)
		}
	}
}

func TestGenericWindowParsing(t *testing.T) {
	used := func(v float64) *float64 { return &v }
	cases := []struct {
		name      string
		in        magpieWindow
		ok        bool
		wantLabel string
		wantMins  int
		wantModel string
		wantGroup string
		wantTitle string
	}{
		{name: "5 hours", in: magpieWindow{Name: "5 hours", Used: used(10)}, ok: true, wantLabel: "5h", wantMins: 300},
		{name: "5h short", in: magpieWindow{Name: "5h", Used: used(10)}, ok: true, wantLabel: "5h", wantMins: 300},
		{name: "7 days", in: magpieWindow{Name: "7 days", Used: used(20)}, ok: true, wantLabel: "7d", wantMins: 10080},
		{name: "weekly", in: magpieWindow{Name: "Weekly", Used: used(20)}, ok: true, wantLabel: "7d", wantMins: 10080},
		{name: "1 day", in: magpieWindow{Name: "1 day", Used: used(30)}, ok: true, wantLabel: "1d", wantMins: 1440},
		{name: "daily", in: magpieWindow{Name: "Daily", Used: used(30)}, ok: true, wantLabel: "1d", wantMins: 1440},
		{name: "monthly", in: magpieWindow{Name: "Monthly", Used: used(40)}, ok: true, wantLabel: "30d", wantMins: 43200},
		{name: "period 31 days normalizes to 30d", in: magpieWindow{Name: "API · Billing", Period: 31 * 86400, Used: used(40)}, ok: true, wantLabel: "30d", wantMins: 43200, wantModel: "API · Billing", wantTitle: "API · Billing"},
		{name: "compound suffix", in: magpieWindow{Name: "7 days · Sonnet", Used: used(45)}, ok: true, wantLabel: "7d", wantMins: 10080, wantModel: "Sonnet", wantTitle: "Sonnet"},
		{name: "compound prefix", in: magpieWindow{Name: "MCP · Month", Used: used(55)}, ok: true, wantLabel: "30d", wantMins: 43200, wantModel: "MCP", wantTitle: "MCP"},
		{name: "pool with window field", in: magpieWindow{Name: "Gemini · 5 hours", Window: "5 hours", Pool: "Gemini", Used: used(15)}, ok: true, wantLabel: "5h", wantMins: 300, wantModel: "Gemini", wantGroup: "Gemini", wantTitle: "Gemini"},
		{name: "embedded space span", in: magpieWindow{Name: "Weekly Opus", Used: used(25)}, ok: true, wantLabel: "7d", wantMins: 10080, wantModel: "Opus", wantTitle: "Opus"},
		{name: "parenthesized span", in: magpieWindow{Name: "Pro Pool (5h)", Used: used(35)}, ok: true, wantLabel: "5h", wantMins: 300, wantModel: "Pro Pool", wantTitle: "Pro Pool"},
		{name: "non-periodic rejected", in: magpieWindow{Name: "Credits", Used: used(10)}, ok: false},
		{name: "multi-span prose rejected", in: magpieWindow{Name: "5-hour and weekly limits", Used: used(0)}, ok: false},
		{name: "text window rejected", in: magpieWindow{Name: "5 hours", Used: used(0), Text: true}, ok: false},
		{name: "unlimited window rejected", in: magpieWindow{Name: "7 days", Used: used(0), Unlimited: true}, ok: false},
		{name: "wallet balance rejected", in: magpieWindow{Name: "Monthly", Used: used(0), Balance: used(100)}, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := parseAccountWindows([]magpieWindow{tc.in})
			if !tc.ok {
				if len(ws) != 0 {
					t.Fatalf("expected rejected, got %+v", ws)
				}
				return
			}
			if len(ws) != 1 {
				t.Fatalf("expected 1 window, got %+v", ws)
			}
			got := ws[0]
			if got.Label != tc.wantLabel || got.WindowMinutes != tc.wantMins || got.Model != tc.wantModel || got.Group != tc.wantGroup || got.Title != tc.wantTitle {
				t.Fatalf("got %+v, want label=%q mins=%d model=%q group=%q title=%q", got, tc.wantLabel, tc.wantMins, tc.wantModel, tc.wantGroup, tc.wantTitle)
			}
		})
	}

	// Multiple windows sharing the same non-pool scope are grouped together.
	grouped := parseAccountWindows([]magpieWindow{
		{Name: "MCP · Month", Used: used(40)},
		{Name: "MCP · 5 hours", Used: used(10)},
	})
	if len(grouped) != 2 || grouped[0].Label != "5h" || grouped[0].Group != "MCP" || grouped[1].Label != "30d" || grouped[1].Group != "MCP" {
		t.Fatalf("expected MCP group sorted shortest-first, got %+v", grouped)
	}
}

func TestNormalizeProviderAndPlan(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"claude", "claude"},
		{"Claude", "claude"},
		{"agy", "antigravity"},
		{"Google Antigravity", "google-antigravity"},
		{"office/claude", "office-claude"},
		{"office/~keys", ""},
		{"---", ""},
	} {
		if got := normalizeProvider(tc.in); got != tc.want {
			t.Errorf("normalizeProvider(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		in, want string
		anon     bool
	}{
		{"max", "Max", false},
		{"promax", "Promax", false},
		{"pro_max", "Promax", false},
		{"pro", "Pro", false},
		{"max 5x", "Max 5x", false},
		{"max_20x", "Max 20x", false},
		{"vip", "VIP", false},
		{"coding plan", "Coding Plan", false},
		{"已登录", "", false},
		{"No plan", "", false},
		{"custom-org-tier", "", true},
		{"max", "Max", true},
	} {
		if got := normalizePlan(tc.in, tc.anon); got != tc.want {
			t.Errorf("normalizePlan(%q, %v) = %q, want %q", tc.in, tc.anon, got, tc.want)
		}
	}
}

func TestRemoteAuthentication(t *testing.T) {
	for _, tc := range []struct {
		base, key       string
		remote, allowed bool
	}{
		{"https://magpie.example.invalid/prefix", "fixture", true, true},
		{"https://magpie.example.invalid", "", true, false},
		{"http://magpie.example.invalid", "fixture", true, false},
		{"https://127.0.0.1:3425", "fixture", true, false},
		{"https://magpie.example.invalid", "fixture", false, false},
	} {
		calls := 0
		client := &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Header.Get("Authorization") != "Bearer fixture" || req.URL.Path != "/prefix"+quotaPath {
				t.Error("wrong remote scope or authorization")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
		})}
		_, _, err := fetchQuotas(context.Background(), client, tc.base, tc.remote, tc.key)
		if (err == nil) != tc.allowed || (calls == 1) != tc.allowed {
			t.Fatalf("base %s: requests=%d err=%v", tc.base, calls, err)
		}
	}
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestIDsSurviveRestartReorderingAndAliasChanges(t *testing.T) {
	dir := t.TempDir()
	var secrets [2][]byte
	var wg sync.WaitGroup
	for i := range secrets {
		wg.Go(func() {
			var err error
			secrets[i], err = accountSecret(dir)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(secrets[0]) != 32 || string(secrets[0]) != string(secrets[1]) {
		t.Fatal("initializers did not agree on a persistent secret")
	}
	first, err := mapQuotas(testQuotas, secrets[0], "same-gateway", "en", mapOptions{})
	if err != nil || len(first) != 5 {
		t.Fatalf("first: %v, %v", first, err)
	}
	const renamedAlias = `{"data":[{"kind":"subscription","provider":"claude","alias":"Renamed Label","user":"two@example.invalid","name":"Claude Code","readAt":"2026-11-25T10:00:00Z","windows":[{"name":"7 days","used":60}]}]}`
	second, err := mapQuotas(renamedAlias, secrets[1], "same-gateway", "en", mapOptions{})
	if err != nil || len(second) != 1 {
		t.Fatalf("next: %v, %v", second, err)
	}
	if second[0].Name != "Renamed Label" {
		t.Fatalf("expected updated alias as display name, got %q", second[0].Name)
	}
	for _, row := range first {
		if row.Provider == "claude" && row.Name == "two@example.invalid" && row.ID == second[0].ID {
			return
		}
	}
	t.Fatal("the same account was assigned a different ID after reordering or alias change")
}

func TestBadAndEmptyResponses(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":null}`, `<html>`, `{"data":{}}`} {
		if _, err := mapQuotas(body, nil, "", "en", mapOptions{}); err == nil {
			t.Fatalf("accepted malformed body %q", body)
		}
	}
	if accounts, err := mapQuotas(`{"data":[]}`, nil, "", "en", mapOptions{}); err != nil || accounts == nil || len(accounts) != 0 {
		t.Fatalf("empty response: %v, %v", accounts, err)
	}
}
