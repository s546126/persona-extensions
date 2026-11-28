package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestManifestAndTranslations(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != extensionID || m.Version != version || m.Engines == nil || m.Engines.PluginProtocol != 1 ||
		m.Icon != "" || len(m.Tags) != 0 || m.Permissions == nil ||
		!slices.Equal(m.Permissions.Network, []string{apiHost, storeHost}) {
		t.Fatal("runtime manifest differs from the extension contract")
	}
	if m.Contributes == nil || len(m.Contributes.Connectors) != 1 || len(m.Contributes.UI) != 2 {
		t.Fatal("missing contribution declarations")
	}
	if sources := m.Contributes.MemorySources; len(sources) != 1 || sources[0].ID != memorySourceID || sources[0].Name == "" {
		t.Fatalf("invalid memory source contribution: %+v", sources)
	}
	c := m.Contributes.Connectors[0]
	if c.ID != extensionID || !slices.Equal(c.Capabilities, []string{"status", "tools", "events", "inbox", "fallback"}) ||
		!slices.Equal(c.Hosts, []string{storeHost, "steamcommunity.com"}) || !slices.Equal(c.AgentTools, []string{"steam_*"}) ||
		!slices.Equal(c.Kinds, []string{wishlistPriceKind, wishlistGiveawayKind, wishlistReleaseKind, wishlistDemoKind, wishlistRoundupKind}) ||
		!slices.Equal(m.ActivationEvents, []string{wire.ActivationEventOnStartup}) {
		t.Fatalf("connector %+v", c)
	}
	page, card := m.Contributes.UI[0], m.Contributes.UI[1]
	if page.ID != extensionID || page.Placement != wire.PlacementNavPage || page.Icon != "chart" ||
		page.Group == nil || page.Group.ID != "personal" || page.Group.Title != "个人" ||
		card.ID != summaryID || card.Placement != wire.PlacementOverviewCard || card.Opens != page.ID {
		t.Fatal("navigation and card declarations do not match")
	}
	if m.Auth == nil || m.Auth.Title == "" || len(m.Secrets) != 1 || m.Secrets[0].Name != keyName || m.Secrets[0].Required {
		t.Fatal("the Hub must offer sign-in with an optional API key")
	}
	var cfg struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	var mode struct {
		Type    string          `json:"type"`
		Enum    []string        `json:"enum"`
		Default json.RawMessage `json:"default"`
	}
	if json.Unmarshal(m.Configuration, &cfg) != nil || json.Unmarshal(cfg.Properties["auth_mode"], &mode) != nil ||
		len(cfg.Required) != 0 || mode.Type != "string" || !slices.Equal(mode.Enum, []string{modeQR, modeWebAPIKey}) || mode.Default != nil {
		t.Fatal("mode selection must preserve the runtime default and let both modes start")
	}

	for _, lang := range []string{"en", "zh-Hans"} {
		raw, err := os.ReadFile(filepath.Join("l10n", lang+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var translation struct {
			Name          string
			Description   string
			Configuration map[string]struct{ Title, Description string }
			Secrets       map[string]struct{ Title, Description string }
			UI            map[string]string
			Groups        map[string]string
		}
		if err := json.Unmarshal(raw, &translation); err != nil {
			t.Fatal(err)
		}
		if translation.Name == "" || translation.Description == "" || translation.UI[page.ID] == "" ||
			translation.UI[card.ID] == "" || translation.Groups[page.Group.ID] == "" {
			t.Errorf("incomplete %s manifest translations", lang)
		}
		for _, key := range []string{"auth_mode", "steam_id", "country_code", "friends_radar_enabled", "discovery_tools_enabled", "discovery_max_results",
			"inbox_events_enabled", "inbox_max_events_per_day", "sale_roundup_enabled", "wishlist_event_triggers", "wishlist_discount_threshold", "wishlist_target_prices"} {
			if f := translation.Configuration[key]; f.Title == "" || f.Description == "" {
				t.Errorf("missing %s translation for %s", lang, key)
			}
		}
		if f := translation.Secrets[keyName]; f.Title == "" || f.Description == "" {
			t.Errorf("missing %s secret translation", lang)
		}
	}
}

func TestSetupAndProtocolConformance(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	t.Setenv("PERSONA_EXTENSION_DATA_DIR", t.TempDir())
	exttest.Check(t, manifest, exttest.Serve(t, program(f.http, time.Now)), json.RawMessage(fmt.Sprintf(`{"steam_id":%q}`, fixtureID)))
	if f.hit(playerPath) != 0 {
		t.Fatal("missing credentials still triggered a request")
	}
}

func TestSteamToolFeatureGateMatrix(t *testing.T) {
	for _, tc := range []struct{ discovery, friends bool }{
		{false, false}, {false, true}, {true, false}, {true, true},
	} {
		t.Run(fmt.Sprintf("discovery=%t/friends=%t", tc.discovery, tc.friends), func(t *testing.T) {
			f := newFakeSteam(t, time.Second)
			dir := t.TempDir()
			settings := fmt.Sprintf(`"discovery_tools_enabled":%t,"friends_radar_enabled":%t,"inbox_events_enabled":false`, tc.discovery, tc.friends)
			h := startInboxTestHost(t, f, &app{}, time.Now, dir, "en", settings)
			h.Answer("model.complete", func(json.RawMessage) (any, error) {
				t.Error("listing tools or invoking gated features called a model")
				return nil, ext.Unsupported("The test hub has no model.")
			})

			p := wire.ConnectorParams{Contribution: extensionID}
			var list wire.ConnectorTools
			if err := h.Call("connector.tools.list", p, &list); err != nil {
				t.Fatal(err)
			}
			listed := make(map[string]bool, len(list.Tools))
			for _, tool := range list.Tools {
				if listed[tool.Name] {
					t.Fatalf("duplicate tool %s", tool.Name)
				}
				listed[tool.Name] = true
			}
			if !listed[wishlistDealsName] || !listed[playtimeRecapName] {
				t.Fatal("the on-demand wishlist deals and recap tools should always be listed")
			}
			for _, group := range []struct {
				names    []string
				enabled  bool
				disabled problem
			}{
				{[]string{storeTagsTool, storeSearchTool, gameDetailsTool}, tc.discovery, problemDiscoveryDisabled},
				{[]string{friendsRadarTool, coopOverlapTool}, tc.friends, problemFriendsDisabled},
			} {
				for _, name := range group.names {
					if listed[name] != group.enabled {
						t.Fatalf("%s listed = %t, want %t", name, listed[name], group.enabled)
					}
					if group.enabled {
						continue
					}
					var out wire.ConnectorCallResult
					err := h.Call("connector.tools.call", wire.ConnectorCallParams{
						Contribution: extensionID, Name: name, Args: json.RawMessage(`{}`),
					}, &out)
					var refusal *ext.Error
					if !errors.As(err, &refusal) || refusal.Code != string(group.disabled) || out.Text != "" {
						t.Fatalf("disabled %s returned %q and %v", name, out.Text, err)
					}
				}
			}
			skillPath := filepath.Join(dir, discoverySkillDir, discoverySkillID, "SKILL.md")
			_, skillErr := os.Stat(skillPath)
			if tc.discovery && skillErr != nil || !tc.discovery && !os.IsNotExist(skillErr) {
				t.Fatalf("discovery skill visibility should follow the setting, got %v", skillErr)
			}
			if got := collectWishlistInbox(t, h); len(got) != 0 || hasEvent(h, "connector.changed") {
				t.Fatal("disabled Inbox produced an event")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.hits) != 0 {
				t.Fatalf("listing tools or calling disabled features contacted Steam: %v", f.hits)
			}
		})
	}
}

func startTestHost(t *testing.T, f *fakeSteam, id, country, dataDir string) *exttest.Host {
	t.Helper()
	t.Setenv("PERSONA_EXTENSION_DATA_DIR", dataDir)
	return exttest.StartWith(t, manifest, exttest.Serve(t, program(f.http, time.Now)), func(p *wire.InitializeParams) {
		p.Config = json.RawMessage(fmt.Sprintf(`{"steam_id":%q,"country_code":%q}`, id, country))
		p.Secrets = map[string]string{keyName: f.key}
		p.Hub.Locale = "en"
	})
}

func toolAnswer(t *testing.T, h *exttest.Host, name, args string) string {
	t.Helper()
	var result wire.ConnectorCallResult
	if err := h.Call("connector.tools.call", wire.ConnectorCallParams{
		Contribution: extensionID, Name: name, Args: json.RawMessage(args),
	}, &result); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !json.Valid([]byte(result.Text)) {
		t.Fatalf("%s did not yield a JSON tool result", name)
	}
	return result.Text
}

func viewAnswer(t *testing.T, h *exttest.Host, p wire.UIParams) wire.View {
	t.Helper()
	var raw json.RawMessage
	if err := h.Call("ui.render", p, &raw); err != nil {
		t.Fatal(err)
	}
	if err := exttest.CheckView(raw, p.Size == wire.SizeCompact); err != nil {
		t.Fatal(err)
	}
	var view wire.View
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(&view); err != nil {
		t.Fatal(err)
	}
	return view
}

func act(t *testing.T, h *exttest.Host, p wire.UIParams, v wire.View, action string, values map[string]string) wire.View {
	t.Helper()
	p.Action, p.State, p.Values = action, v.State, values
	var out wire.ActionResult
	if err := h.Call("ui.action", p, &out); err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(out.View); err != nil {
		t.Fatalf("after %s: %v", action, err)
	}
	return *out.View
}

func contains(t *testing.T, haystack string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(haystack, want) {
			t.Errorf("missing %q in the response", want)
		}
	}
}

func TestProtocolTools(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	h := startTestHost(t, f, fixtureVanity, "GB", "")
	var status wire.ConnectorStatus
	if err := h.Call("connector.status", wire.ConnectorParams{Contribution: extensionID}, &status); err != nil ||
		status.State != wire.ConnectorStateOk || f.hit(vanityPath) != 0 || f.hit(playerPath) != 0 {
		t.Fatalf("status must be readable without an upstream request: %+v, %v", status, err)
	}
	var list wire.ConnectorTools
	if err := h.Call("connector.tools.list", wire.ConnectorParams{Contribution: extensionID}, &list); err != nil {
		t.Fatal(err)
	}
	want := []string{"steam_profile", "steam_recent_games", "steam_library", "steam_achievements", "steam_wishlist",
		"steam_achievement_hunter", "steam_game_news", wishlistDealsName, playtimeRecapName,
		"steam_backlog_summary", "steam_backlog_mark", "steam_tonight_pick",
		"steam_store_tags", "steam_store_search", "steam_game_details"}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		if !json.Valid(tool.InputSchema) {
			t.Errorf("invalid schema for %s", tool.Name)
		}
	}
	if !slices.Equal(names, want) {
		t.Fatalf("Steam tools %v, want %v", names, want)
	}

	contains(t, toolAnswer(t, h, "steam_profile", "{}"), `"owned_game_count":3`, `"state":"online"`, `"playing":"Synthetic Adventure"`)
	if err := h.Call("connector.status", wire.ConnectorParams{Contribution: extensionID}, &status); err != nil ||
		status.Account != "Fixture Pilot" || status.LastSync.IsZero() {
		t.Fatalf("status did not report the last known account: %+v, %v", status, err)
	}
	contains(t, toolAnswer(t, h, "steam_recent_games", `{"offset":1,"limit":1}`), `"playtime_2weeks":130`, `"playtime_forever":600`, `"Zeta Rally"`)
	contains(t, toolAnswer(t, h, "steam_library", `{"limit":1,"sort":"playtime"}`), `"next_offset":1`, `"Alpha Café"`, `"owned_game_count":3`)
	contains(t, toolAnswer(t, h, "steam_library", `{"limit":1,"offset":1,"sort":"name"}`), `"next_offset":2`, `"Synthetic Adventure"`)
	contains(t, toolAnswer(t, h, "steam_library", `{"query":"cAfÉ"}`), `"matched_count":1`, `"Alpha Café"`)
	contains(t, toolAnswer(t, h, "steam_library", `{"query":"no matching title"}`), `"matched_count":0`, `"games":[]`)
	contains(t, toolAnswer(t, h, "steam_achievements", fmt.Sprintf(`{"appid":%d}`, firstApp)), `"total":3`, `"unlocked":2`)
	contains(t, toolAnswer(t, h, "steam_wishlist", `{"limit":1}`), `"country_code":"GB"`, `"discounted_count":1`, `"discount_percent":40`, `"currency":"GBP"`, `"final_formatted":"£3.99"`)
	for _, tc := range []struct{ name, args string }{
		{"steam_profile", `{"steam_id":"another_account"}`},
		{"steam_library", `{"sort":"currency"}`},
		{"steam_library", `{"limit":51}`},
		{"steam_recent_games", `{"offset":-1}`},
		{"steam_wishlist", `{"country_code":"JP"}`},
		{"steam_achievements", `{"appid":0}`},
		{"steam_achievements", `{"appid":4294967296}`},
		{achievementHunterName, fmt.Sprintf(`{"appid":%d,"limit":51}`, firstApp)},
		{gameNewsName, `{"appid":0}`},
		{gameNewsName, `{"limit":21}`},
		{gameNewsName, `{"games_limit":6}`},
		{gameNewsName, `{"summary_enabled":true}`},
		{wishlistDealsName, `{"limit":51}`},
		{wishlistDealsName, `{"steam_id":"another_account"}`},
	} {
		var e *ext.Error
		err := h.Call("connector.tools.call", wire.ConnectorCallParams{Contribution: extensionID, Name: tc.name, Args: json.RawMessage(tc.args)}, nil)
		if !errors.As(err, &e) || e.Code != "invalid" {
			t.Errorf("%s accepted invalid arguments: %v", tc.name, err)
		}
	}
	var e *ext.Error
	if err := h.Call("connector.actions.list", wire.ConnectorParams{Contribution: extensionID}, nil); !errors.As(err, &e) || e.Code != wire.ErrorCodeUnknownMethod {
		t.Fatalf("write namespace was enabled: %v", err)
	}
}

func TestDeclarativeViews(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	h := startTestHost(t, f, fixtureID, "", dir)
	nav := wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, UIVersion: 12, Locale: "en-US"}
	card := wire.UIParams{Contribution: summaryID, Size: wire.SizeCompact, Placement: wire.PlacementOverviewCard, UIVersion: 12, Locale: "zh-Hans-CN"}
	v := viewAnswer(t, h, nav)
	raw, _ := json.Marshal(v)
	contains(t, string(raw), "Playing Synthetic Adventure", "2 h 10 min", "Fixture Pilot", "40% off", "$3.99")
	if len(v.Sections) != 5 || v.Sections[4].Title != english.backlog.title ||
		v.Search == nil || v.Filter == nil || v.Sections[0].Items[1].Stats[1].Value != "3" {
		t.Fatal("navigation is missing a section or a display control")
	}
	if _, err := os.Stat(filepath.Join(dir, "steam-cache-v1", "profile.json")); err != nil {
		t.Fatalf("configured environment did not receive the persistent cache: %v", err)
	}
	v = act(t, h, nav, v, v.Search.Action.ID, map[string]string{"q": "rally"})
	library := v.Sections[2]
	if len(library.Items) != 1 || library.Items[0].Title != "Zeta Rally" {
		t.Fatal("library search did not filter the displayed games")
	}
	v = act(t, h, nav, v, v.Filter.Action.ID, map[string]string{"q": "", "order": sortName})
	if v.Filter.Value != sortName || v.Sections[2].Items[0].Title != "Alpha Café" {
		t.Fatal("library name sort was not applied")
	}
	fromLibrary := act(t, h, nav, v, v.Sections[2].Items[1].Action.ID, nil)
	fromRecent := act(t, h, nav, v, v.Sections[1].Items[0].Action.ID, nil)
	for _, detail := range []wire.View{fromLibrary, fromRecent} {
		raw, _ := json.Marshal(detail)
		contains(t, string(raw), "Synthetic Adventure", "2 of 3 unlocked", storeLink(firstApp))
		back := act(t, h, nav, detail, "back", nil)
		if back.Filter == nil || back.Filter.Value != sortName {
			t.Fatal("back lost the library state")
		}
	}
	raw, _ = json.Marshal(viewAnswer(t, h, card))
	contains(t, string(raw), "游戏中", "近两周", "2 小时 10 分钟", "愿望单打折", `"value":"1"`)

	// Warm the page's locale before testing a failed refresh of that snapshot.
	viewAnswer(t, h, nav)
	beforeRefresh := map[string]int{}
	for _, path := range []string{playerPath, ownedPath, recentPath, wishlistPath, storePricesPath} {
		beforeRefresh[path] = f.hit(path)
	}
	f.reply(storePricesPath, 429, f.key)
	v = act(t, h, nav, v, "refresh", nil)
	raw, _ = json.Marshal(v)
	contains(t, string(raw), "Showing cached data.", "rate limiting")
	if strings.Contains(string(raw), f.key) {
		t.Fatal("refresh reflected a secret from an upstream error")
	}
	for path, before := range beforeRefresh {
		if f.hit(path) != before+1 {
			t.Errorf("%s was not explicitly refreshed", path)
		}
	}
	v = act(t, h, nav, v, "search", map[string]string{"q": f.key})
	raw, _ = json.Marshal(v)
	if strings.Contains(string(raw), f.key) {
		t.Fatal("search reflected the credential")
	}
}

func TestWishlistRowShowsOnlyPositiveDiscount(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	h := startTestHost(t, f, fixtureID, "", "")
	for _, tc := range []struct{ locale, discount string }{
		{"en-US", "40% off"},
		{"zh-Hans", "优惠 40%"},
	} {
		t.Run(tc.locale, func(t *testing.T) {
			v := viewAnswer(t, h, wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, UIVersion: 12, Locale: tc.locale})
			rows := v.Sections[3].Items
			if sale := rows[0]; sale.Value != "$3.99" || sale.Badge == nil || sale.Badge.Text != tc.discount {
				t.Errorf("sale row should show the price and localized discount badge, got %q and %+v", sale.Value, sale.Badge)
			}
			if full := rows[1]; full.Value != "$6.99" || full.Subtitle != "" || full.Badge != nil {
				t.Errorf("full-price row should show only the price, got %q, subtitle %q, and badge %+v", full.Value, full.Subtitle, full.Badge)
			}
		})
	}
}

func TestWishlistNamesFollowLocaleAndCacheScope(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	f.handle(storeNamesPath, func(w http.ResponseWriter, r *http.Request) {
		var query struct {
			Context struct {
				Language    string `json:"language"`
				CountryCode string `json:"country_code"`
			} `json:"context"`
		}
		if err := json.Unmarshal([]byte(r.URL.Query().Get("input_json")), &query); err != nil || query.Context.CountryCode != "GB" {
			t.Error("expected a StoreBrowse request for GB")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var name string
		switch query.Context.Language {
		case "english":
			name = "Fixture Adventure"
		case "schinese":
			name = "示例冒险"
		default:
			t.Errorf("unexpected StoreBrowse language %q", query.Context.Language)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `{"response":{"store_items":[{"appid":%d,"name":%q,"success":1}]}}`, firstApp, name)
	})
	dir := t.TempDir()
	h := startTestHost(t, f, fixtureID, "GB", dir)
	contains(t, toolAnswer(t, h, "steam_wishlist", `{"limit":1}`), "Fixture Adventure")
	firstTitle := func(h *exttest.Host, locale string) string {
		t.Helper()
		v := viewAnswer(t, h, wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, UIVersion: 12, Locale: locale})
		return v.Sections[3].Items[0].Title
	}
	if title := firstTitle(h, "ja-JP"); title != "Fixture Adventure" || f.hit(storeNamesPath) != 1 {
		t.Fatalf("non-Chinese locale should reuse the English snapshot, got %q after %d requests", title, f.hit(storeNamesPath))
	}
	if title := firstTitle(h, "zh-Hans"); title != "示例冒险" || f.hit(storeNamesPath) != 2 {
		t.Fatalf("Chinese locale should fetch a localized snapshot, got %q after %d requests", title, f.hit(storeNamesPath))
	}
	h = startTestHost(t, f, fixtureID, "GB", dir)
	if title := firstTitle(h, "zh_CN"); title != "示例冒险" || f.hit(storeNamesPath) != 2 {
		t.Fatalf("Chinese locale variants should reuse the persisted snapshot, got %q after %d requests", title, f.hit(storeNamesPath))
	}
	contains(t, toolAnswer(t, h, "steam_wishlist", `{"limit":1}`), "Fixture Adventure")
	if hits := f.hit(storeNamesPath); hits != 3 {
		t.Fatalf("switching back to English should read the English name, got %d requests", hits)
	}
	f.reply(wishlistPath, http.StatusForbidden, "hidden")
	if title := firstTitle(h, "zh-Hans"); title != chinese.message(problemWishlist, false) {
		t.Fatalf("Chinese wishlist should report the privacy change, got %q", title)
	}
	contains(t, toolAnswer(t, h, "steam_wishlist", `{"limit":1}`), `"available":false`, `"wishlist_unavailable"`)
}

func TestHiddenAndPartialReads(t *testing.T) {
	t.Run("private profile", func(t *testing.T) {
		f := newFakeSteam(t, time.Second)
		f.reply(playerPath, 200, fmt.Sprintf(`{"response":{"players":[{"steamid":%q,"personaname":"Fixture Pilot","communityvisibilitystate":1,"gameextrainfo":"Hidden Game"}]}}`, fixtureID))
		h := startTestHost(t, f, fixtureID, "", "")
		contains(t, toolAnswer(t, h, "steam_profile", "{}"), `"public":false`, `"private_profile"`)
		for _, tool := range []string{"steam_library", "steam_recent_games", "steam_wishlist"} {
			contains(t, toolAnswer(t, h, tool, "{}"), `"available":false`, `"private_profile"`)
		}
		v := viewAnswer(t, h, wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, UIVersion: 12})
		raw, _ := json.Marshal(v)
		if strings.Contains(string(raw), "Hidden Game") || f.hit(ownedPath) != 0 || f.hit(wishlistPath) != 0 {
			t.Fatal("private profile exposed gameplay or fetched additional data")
		}
	})

	t.Run("game details and wishlist hidden separately", func(t *testing.T) {
		f := newFakeSteam(t, time.Second)
		f.reply(ownedPath, 200, `{"response":{}}`)
		f.reply(wishlistPath, 200, `{"response":{}}`)
		f.reply(achievementsPath, http.StatusForbidden, "hidden")
		h := startTestHost(t, f, fixtureID, "", "")
		contains(t, toolAnswer(t, h, "steam_library", "{}"), `"available":false`, `"game_details_unavailable"`)
		contains(t, toolAnswer(t, h, "steam_wishlist", "{}"), `"available":false`, `"wishlist_unavailable"`)
		contains(t, toolAnswer(t, h, "steam_recent_games", "{}"), `"available":true`, `"playtime_2weeks":130`)
		contains(t, toolAnswer(t, h, "steam_achievements", fmt.Sprintf(`{"appid":%d}`, firstApp)), `"achievements_unavailable"`)
		if f.hit(storePricesPath) != 0 {
			t.Fatal("private wishlist initiated a Store lookup")
		}
	})

	t.Run("cold Store failure", func(t *testing.T) {
		f := newFakeSteam(t, time.Second)
		f.reply(storePricesPath, 429, "rate limit")
		h := startTestHost(t, f, fixtureID, "", "")
		contains(t, toolAnswer(t, h, "steam_wishlist", "{}"), `"available":true`, `"prices_checked":0`, `"rate_limited"`)
		card := viewAnswer(t, h, wire.UIParams{Contribution: summaryID, Size: wire.SizeCompact, Placement: wire.PlacementOverviewCard, UIVersion: 12})
		if got := card.Sections[0].Items[1].Stats[1].Value; got != "—" {
			t.Errorf("unknown discounts shown as %q", got)
		}
	})
}

func TestInvalidSteamConfiguration(t *testing.T) {
	for _, test := range []struct {
		id      string
		country string
		issue   problem
	}{
		{"", "", problemSetup},
		{"00000000000000000", "", problemInvalidID},
		{"https://example.com/id/unsafe", "", problemInvalidID},
		{fixtureID, "ZZ", problemCountry},
		{fixtureID, "gb", ""},
	} {
		c, issue := (config{AuthMode: modeWebAPIKey, SteamID: test.id, CountryCode: test.country}).normalized("synthetic-key")
		if issue != test.issue || (issue == "" && c.CountryCode != "GB") {
			t.Errorf("configuration issue=%q country=%q, expected issue %q", issue, c.CountryCode, test.issue)
		}
	}
	f := newFakeSteam(t, time.Second)
	f.reply(vanityPath, 200, fmt.Sprintf(`{"response":{"success":42,"message":%q}}`, f.key))
	h := startTestHost(t, f, fixtureVanity, "", "")
	out := toolAnswer(t, h, "steam_profile", "{}")
	contains(t, out, `"invalid_steam_id"`, `"available":false`)
	if strings.Contains(out, f.key) || f.hit(playerPath) != 0 {
		t.Fatal("invalid vanity resolution leaked upstream content or fetched the profile")
	}
}

func TestNavigationPagerAndEmptySnapshots(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	games := make([]game, pageSize+2)
	for i := range games {
		games[i] = game{AppID: firstApp + uint32(i), Name: fmt.Sprintf("Fixture %02d", i), PlaytimeForever: uint32(i)}
	}
	payload, err := json.Marshal(map[string]any{"response": map[string]any{"game_count": len(games), "games": games}})
	if err != nil {
		t.Fatal(err)
	}
	f.reply(ownedPath, http.StatusOK, string(payload))
	f.reply(recentPath, http.StatusOK, `{"response":{"total_count":0}}`)
	f.reply(wishlistPath, http.StatusOK, `{"response":{"items":[]}}`)
	h := startTestHost(t, f, fixtureID, "", "")
	p := wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, Locale: "en", UIVersion: 12}
	v := viewAnswer(t, h, p)
	if v.Sections[1].Items[0].Title != english.noRecent || v.Sections[3].Items[0].Title != english.noWishes {
		t.Fatal("explicit empty lists were treated as hidden")
	}
	if v.Sections[2].Items[0].Title != "Fixture 13" || v.Sections[2].Items[pageSize].Action.ID != "library-next" {
		t.Fatal("expected the first library page sorted by playtime")
	}
	v = act(t, h, p, v, "library-next", nil)
	if len(v.Sections[2].Items) != 3 || v.Sections[2].Items[0].Title != "Fixture 01" ||
		v.Sections[2].Items[2].Action.ID != "library-previous" {
		t.Fatal("expected only the remaining games on the last page")
	}
	v = act(t, h, p, v, "library-previous", nil)
	if v.Sections[2].Items[0].Title != "Fixture 13" || f.hit(ownedPath) != 1 || f.hit(storePricesPath) != 0 {
		t.Fatal("paging should reuse the local library, with no Store reads for an empty wishlist")
	}
	contains(t, toolAnswer(t, h, "steam_wishlist", "{}"), `"wishlist_count":0`, `"items":[]`, `"discounted_count":0`)
}
