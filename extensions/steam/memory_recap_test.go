package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestPlaytimeRecapPeriodsFromLocalHistory(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	now := time.Date(2025, time.December, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	a := &app{}
	h := startInboxTestHost(t, f, a, clock, dir, "en", `"discovery_tools_enabled":false`)
	const newApp = thirdApp + 1
	setLibrary := func(games ...game) {
		raw, err := json.Marshal(games)
		if err != nil {
			t.Fatal(err)
		}
		f.reply(ownedPath, 200, fmt.Sprintf(`{"response":{"game_count":%d,"games":%s}}`, len(games), raw))
	}
	setLibrary(game{AppID: firstApp, Name: "Fixture Quest", PlaytimeForever: 100}, game{AppID: secondApp, Name: "Fixture Rally"}, game{AppID: thirdApp, Name: "Fixture Maze"})
	mark := func(appID uint32, status BacklogStatus) {
		t.Helper()
		answer := toolAnswer(t, h, backlogMarkTool, fmt.Sprintf(`{"appid":%d,"status":%q}`, appID, status))
		contains(t, answer, `"available":true`)
	}
	mark(firstApp, BacklogPlaying)
	mark(secondApp, BacklogQueued)
	mark(thirdApp, BacklogUnplayed)
	fallback := toolAnswer(t, h, "steam_playtime_recap", `{}`)
	contains(t, fallback, `"period":"month"`, `"owned_games":3`, `"total_minutes":100`, `"playtime_2weeks":130`,
		`"playing":1`, `"queued":1`, "start snapshot", "automatically")
	assertHidden(t, fallback, `"delta":`)

	now = time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	setLibrary(game{AppID: firstApp, Name: "Fixture Quest", PlaytimeForever: 150}, game{AppID: secondApp, Name: "Fixture Rally"}, game{AppID: thirdApp, Name: "Fixture Maze"})
	toolAnswer(t, h, "steam_playtime_recap", `{}`)
	now = time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	setLibrary(game{AppID: firstApp, Name: "Fixture Quest", PlaytimeForever: 250}, game{AppID: secondApp, Name: "Fixture Rally"}, game{AppID: thirdApp, Name: "Fixture Maze"})
	toolAnswer(t, h, "steam_playtime_recap", `{}`)
	now = time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	setLibrary(game{AppID: firstApp, Name: "Fixture Quest", PlaytimeForever: 300}, game{AppID: secondApp, Name: "Fixture Rally"}, game{AppID: thirdApp, Name: "Fixture Maze"})
	toolAnswer(t, h, "steam_playtime_recap", `{}`)

	now = time.Date(2026, time.November, 27, 12, 0, 0, 0, time.UTC)
	setLibrary(game{AppID: firstApp, Name: "Fixture Quest", PlaytimeForever: 340}, game{AppID: secondApp, Name: "Fixture Rally", PlaytimeForever: 120}, game{AppID: newApp, Name: "Fixture Castle", PlaytimeForever: 10})
	mark(firstApp, BacklogCompleted)
	mark(secondApp, BacklogPlaying)
	mark(newApp, BacklogDropped)
	// Start a fresh process to prove the comparison survives a restart.
	h = startInboxTestHost(t, f, &app{}, clock, dir, "en", `"discovery_tools_enabled":false`)
	for _, tc := range []struct {
		period string
		start  string
		mins   int
	}{
		{"month", "2026-11-01", 170}, {"quarter", "2026-10-02", 220}, {"year", "2026-01-01", 320}, {"all", "2025-12-10", 370},
	} {
		answer := toolAnswer(t, h, "steam_playtime_recap", fmt.Sprintf(`{"period":%q,"top_n":1}`, tc.period))
		contains(t, answer, `"from":"`+tc.start, fmt.Sprintf(`"playtime_minutes":%d`, tc.mins), `"new_games":1`,
			`"top_games":[`, `"backlog_at_start":2`, `"backlog_consumed":1`, `"backlog_consumption_rate":0.5`,
			`"count_changes":{"backlog":-2,"completed":1,"dropped":1,"playing":0}`,
			`"moved_into":{"backlog":0,"completed":1,"dropped":1,"playing":1}`)
		var result playtimeRecap
		if err := json.Unmarshal([]byte(answer), &result); err != nil {
			t.Fatal(err)
		}
		want := firstApp
		if tc.period == "month" || tc.period == "quarter" {
			want = secondApp
		}
		if result.Delta == nil || len(result.Delta.TopGames) != 1 || result.Delta.TopGames[0].AppID != want {
			t.Errorf("%s top game is incorrect: %s", tc.period, answer)
		}
	}
	path := filepath.Join(dir, storeDirectory, "playtime-recap.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	toolAnswer(t, h, "steam_playtime_recap", `{}`)
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || f.hit(ownedPath) != 5 {
		t.Fatalf("same-day recaps should reuse the library and skip disk replacement: %v, %d library reads", err, f.hit(ownedPath))
	}
	nav := wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, UIVersion: 12, Locale: "en"}
	view := act(t, h, nav, viewAnswer(t, h, nav), recapAction, nil)
	view = act(t, h, nav, view, recapPeriodAction, map[string]string{"period": "year"})
	page, _ := json.Marshal(view)
	contains(t, string(page), "Observed 2026-01-01 through 2026-11-27", "5 h 20 min", "50%", "Fixture Quest")
}

func TestPlaytimeRecapPageLoadsOnDemand(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	h := startTestHost(t, f, fixtureID, "GB", dir)
	nav := wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, UIVersion: 12, Locale: "zh-Hans"}
	first := viewAnswer(t, h, nav)
	if _, err := os.Stat(filepath.Join(dir, storeDirectory, "playtime-recap.json")); !os.IsNotExist(err) {
		t.Fatalf("overview should skip recap history: %v", err)
	}
	var action string
	for _, item := range first.Sections[0].Items {
		if item.Title == "游玩复盘" && item.Type == wire.ComponentButton && item.Action != nil {
			action = item.Action.ID
		}
	}
	if action == "" {
		t.Fatal("the overview is missing the recap button")
	}
	filtered := act(t, h, nav, first, "search", map[string]string{"q": "rally"})
	recap := act(t, h, nav, filtered, action, nil)
	if recap.Title != "游玩复盘" || recap.Filter == nil || recap.Filter.Value != "month" || len(recap.Filter.Choices) != 4 {
		t.Fatalf("invalid recap navigation: %+v", recap)
	}
	raw, _ := json.Marshal(recap)
	contains(t, string(raw), "周期起点快照", "自动累计", chinese.recentTime)
	for _, period := range []string{"quarter", "year", "all", "month"} {
		recap = act(t, h, nav, recap, recap.Filter.Action.ID, map[string]string{recap.Filter.Name: period})
		if recap.Filter == nil || recap.Filter.Value != period {
			t.Fatalf("period did not change to %s", period)
		}
	}
	if f.hit(ownedPath) != 1 || f.hit(recentPath) != 1 || f.hit(wishlistPath) != 1 {
		t.Fatal("recap period changes should reuse the library and recent cache")
	}
	if back := act(t, h, nav, recap, "back", nil); back.Search == nil || back.Search.Value != "rally" {
		t.Fatal("returning from recap lost the parent page's search state")
	}
}

func TestPlaytimeRecapQRHistoryIsSessionOnly(t *testing.T) {
	dir := t.TempDir()
	var clock atomic.Int64
	start := time.Date(2026, time.November, 27, 23, 58, 0, 0, time.UTC)
	clock.Store(start.Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	a := &app{}
	f, h := startRecapQRHost(t, a, dir, now, `{"auth_mode":"qr","discovery_tools_enabled":false}`)
	signIn := func() {
		authStep(t, h, "")
		await(t, 5*time.Second, func() bool { return authStep(t, h, qrStep).Done })
	}
	signIn()
	answer := toolAnswer(t, h, playtimeRecapName, `{}`)
	contains(t, answer, `"available":true`, `"hint":`)
	clock.Add(3 * 60)
	f.reply(ownedPath, http.StatusOK, fmt.Sprintf(`{"response":{"game_count":1,"games":[{"appid":%d,"name":"Fixture Quest","playtime_forever":240}]}}`, firstApp))
	answer = toolAnswer(t, h, playtimeRecapName, `{}`)
	contains(t, answer, `"playtime_minutes":40`, `"new_games":0`)
	if f.hit(ownedPath) != 2 {
		t.Fatal("recap should refresh an overnight library cache on demand")
	}
	for _, name := range []string{storeDirectory, "steam-cache-v1"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("QR recap wrote remote account data under %s: %v", name, err)
		}
	}
	a.qr.mu.Lock()
	previous := a.qr.session.reads
	a.qr.mu.Unlock()
	var status wire.AuthStatus
	if err := h.Call("auth.logout", ext.Empty{}, &status); err != nil || status.State != wire.AuthStateSignedOut {
		t.Fatalf("QR logout failed: %s, %v", status.State, err)
	}
	requireStoreOK(t, previous.stores.playtime.recaps.view(func(entries map[string][]recapSnapshot) {
		if len(entries) != 0 {
			t.Error("the signed-out session retained recap history")
		}
	}))
	answer = toolAnswer(t, h, playtimeRecapName, `{}`)
	contains(t, answer, `"available":false`, string(problemAuthRequired))
	assertHidden(t, answer, `"library":`, `"delta":`)
}

func startRecapQRHost(t *testing.T, a *app, dir string, now func() time.Time, cfg string) (*fakeSteam, *exttest.Host) {
	t.Helper()
	t.Setenv("PERSONA_EXTENSION_DATA_DIR", dir)
	access := fixtureJWT(t, fixtureID, now().Add(time.Hour), "recap-access")
	refresh := fixtureSecret(t, fixtureID, now()).RefreshToken
	f := newFakeSteamMode(t, time.Second, access)
	f.handle(authBasePath+beginQRPath+"/v1/", func(w http.ResponseWriter, _ *http.Request) {
		authFixture(w, "1", beginFixture())
	})
	f.handle(authBasePath+pollQRPath+"/v1/", func(w http.ResponseWriter, _ *http.Request) {
		authFixture(w, "1", grantFixture(refresh, access))
	})
	h := exttest.StartWith(t, manifest, exttest.Serve(t, a.program(f.http, now)), func(p *wire.InitializeParams) {
		p.Config, p.Hub.Locale = json.RawMessage(cfg), "en"
	})
	return f, h
}

func TestPlaytimeRecapRejectsInvalidArgsWithoutReading(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	h := startTestHost(t, f, fixtureID, "GB", t.TempDir())
	for _, args := range []string{`{"period":"week"}`, `{"period":""}`, `{"top_n":0}`, `{"top_n":21}`, `{"other":true}`} {
		var out wire.ConnectorCallResult
		err := h.Call("connector.tools.call", wire.ConnectorCallParams{
			Contribution: extensionID, Name: playtimeRecapName, Args: json.RawMessage(args),
		}, &out)
		var issue *ext.Error
		if !errors.As(err, &issue) || issue.Code != "invalid" || out.Text != "" {
			t.Errorf("invalid recap args %s yielded %q and %v", args, out.Text, err)
		}
	}
	if f.hit(playerPath) != 0 || f.hit(ownedPath) != 0 || f.hit(recentPath) != 0 {
		t.Fatal("invalid recap args contacted Steam")
	}
}

func memoryAnswer(t *testing.T, h *exttest.Host, limit int, after string) wire.MemoryEntriesResult {
	t.Helper()
	var raw json.RawMessage
	if err := h.Call("memory.entries", wire.MemoryEntriesParams{Contribution: memorySourceID, Limit: limit, After: after}, &raw); err != nil {
		t.Fatal(err)
	}
	if err := wire.ValidateJSON("MemoryEntriesResult", raw); err != nil {
		t.Fatalf("invalid memory response: %v", err)
	}
	var out wire.MemoryEntriesResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertEmptyMemory(t *testing.T, got wire.MemoryEntriesResult) {
	t.Helper()
	want := wire.MemoryEntriesResult{Entries: []wire.MemoryEntry{}, Tagged: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected an empty tagged response, got %+v", got)
	}
}

func steamRequests(f *fakeSteam) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int
	for _, count := range f.hits {
		total += count
	}
	return total
}

func TestMemoryEntriesOptOutAndUnconfiguredDoNoWork(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"default off", `"memory_max_items":100`},
		{"empty dimensions", `"memory_export_enabled":true,"memory_export_dimensions":[]`},
		{"missing key setup", `"auth_mode":"web_api_key","memory_export_enabled":true`},
		{"signed out QR", `"auth_mode":"qr","memory_export_enabled":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSteam(t, time.Second)
			dir := t.TempDir()
			requireStoreOK(t, NewBacklogStore(dir).Set(fixtureID, firstApp, BacklogEntry{Status: BacklogPlaying}))
			t.Setenv("PERSONA_EXTENSION_DATA_DIR", dir)
			h := exttest.StartWith(t, manifest, exttest.Serve(t, program(f.http, time.Now)), func(p *wire.InitializeParams) {
				p.Config = json.RawMessage(fmt.Sprintf(`{"steam_id":%q,%s}`, fixtureID, tc.fields))
				if tc.name == "default off" || tc.name == "empty dimensions" {
					p.Secrets = map[string]string{keyName: f.key}
				}
			})
			h.Answer("model.complete", func(json.RawMessage) (any, error) {
				t.Error("an empty memory read called a model")
				return nil, ext.Unsupported("The test hub has no model.")
			})
			assertEmptyMemory(t, memoryAnswer(t, h, 500, "ignored-when-empty"))
			if steamRequests(f) != 0 {
				t.Fatal("an empty memory read contacted Steam")
			}
		})
	}
}

func TestMemoryEntriesValidatesProtocolParams(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	a := &app{}
	startInboxTestHost(t, f, a, time.Now, t.TempDir(), "en", `"memory_export_enabled":true`)
	for _, tc := range []struct {
		params wire.MemoryEntriesParams
		code   string
	}{
		{wire.MemoryEntriesParams{Contribution: memorySourceID, Limit: 0}, "invalid"},
		{wire.MemoryEntriesParams{Contribution: memorySourceID, Limit: 501}, "invalid"},
		{wire.MemoryEntriesParams{Contribution: memorySourceID, Limit: 1, After: strings.Repeat("a", 513)}, "invalid"},
		{wire.MemoryEntriesParams{Contribution: fixtureID, Limit: 1}, wire.ErrorCodeNoContribution},
	} {
		// The test Hub validates RPC parameters before delivery. Send invalid
		// parameters directly to exercise the extension's defensive checks.
		raw, _ := json.Marshal(tc.params)
		_, err := a.memoryEntries(t.Context(), "entries", raw)
		var issue *ext.Error
		if !errors.As(err, &issue) || issue.Code != tc.code {
			t.Fatalf("memory.entries expected %s, got %v", tc.code, err)
		}
		assertHidden(t, issue.Message, fixtureID)
	}
	if steamRequests(f) != 0 {
		t.Fatal("invalid memory arguments contacted Steam")
	}
}

func TestMemoryEntriesCachedProfileAndPagination(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	const note = "Fixture private notes and tags"
	const email = "steam-player@example.test"
	games := []game{
		{AppID: firstApp, Name: "Fixture Quest", PlaytimeForever: 1500},
		{AppID: secondApp, Name: "Fixture Rally", PlaytimeForever: 800},
		{AppID: thirdApp, Name: "Fixture Maze"},
		{AppID: thirdApp + 1, Name: email, PlaytimeForever: 10_000},
		{AppID: thirdApp + 2, Name: fixtureID2, PlaytimeForever: 10_000},
		{AppID: thirdApp + 3, Name: f.key, PlaytimeForever: 10_000},
	}
	raw, _ := json.Marshal(games)
	f.reply(ownedPath, 200, fmt.Sprintf(`{"response":{"games":%s}}`, raw))
	f.reply(recentPath, 200, fmt.Sprintf(`{"response":{"games":[{"appid":%d,"name":"Fixture Rally","playtime_2weeks":60}]}}`, secondApp))
	f.reply(achievementsPath, 200, `{"playerstats":{"success":true,"gameName":"Fixture Quest","achievements":[`+strings.Repeat(`{"achieved":1},`, 9)+`{"achieved":0}]}}`)
	f.reply(storePricesPath, 200, fmt.Sprintf(`{"%d":{"success":true,"data":{"steam_appid":%d,"name":"Fixture Quest","genres":[{"description":"Adventure"}]}}}`, firstApp, firstApp))
	f.reply(appReviewsPath+appIDText(firstApp), 200, `{"success":1,"query_summary":{"total_positive":2,"total_negative":0,"total_reviews":2}}`)
	f.reply(reviewsAPIPath, 200, `{"response":{"total_positive":2,"total_negative":0,"total_reviews":2}}`)
	a := &app{}
	h := startInboxTestHost(t, f, a, time.Now, dir, "en", `"memory_export_enabled":true`)
	requireStoreOK(t, a.backlog.Set(fixtureID, firstApp, BacklogEntry{Status: BacklogPlaying, Note: note, Tags: []string{email}}))
	requireStoreOK(t, a.backlog.Set(fixtureID, thirdApp, BacklogEntry{Status: BacklogCompleted}))
	for _, name := range []string{"steam_library", "steam_recent_games", "steam_achievements", gameDetailsTool} {
		args := `{}`
		if name == "steam_achievements" || name == gameDetailsTool {
			args = fmt.Sprintf(`{"appid":%d}`, firstApp)
		}
		contains(t, toolAnswer(t, h, name, args), `"available":true`)
	}
	requests := steamRequests(f)
	full := memoryAnswer(t, h, 500, "")
	if len(full.Entries) < 7 || !full.Tagged || full.HasMore || full.StandingBytes != 2048 || !full.Entries[0].Standing {
		t.Fatalf("invalid gaming profile: %+v", full)
	}
	profile, _ := json.Marshal(full)
	contains(t, string(profile), "Fixture Quest", "Fixture Rally", "Fixture Maze", "Adventure", "9/10", "25 h")
	assertHidden(t, string(profile), fixtureID, fixtureID2, "Fixture Pilot", email, note, f.key)
	var paged []wire.MemoryEntry
	after := ""
	for range len(full.Entries) {
		page := memoryAnswer(t, h, 1, after)
		if len(page.Entries) != 1 || !page.Tagged || page.HasMore != (page.After != "") || page.After != "" && page.After == after {
			t.Fatalf("invalid memory page: %+v", page)
		}
		paged = append(paged, page.Entries...)
		after = page.After
	}
	if after != "" || !reflect.DeepEqual(paged, full.Entries) || steamRequests(f) != requests {
		t.Fatal("pagination changed ordering or contacted Steam")
	}
	firstPage := memoryAnswer(t, h, 1, "")
	requireStoreOK(t, a.backlog.Set(fixtureID, firstApp, BacklogEntry{Status: BacklogDropped}))
	var invalidPage wire.MemoryEntriesResult
	err := h.Call("memory.entries", wire.MemoryEntriesParams{Contribution: memorySourceID, Limit: 1, After: firstPage.After}, &invalidPage)
	var issue *ext.Error
	if !errors.As(err, &issue) || issue.Code != "invalid" {
		t.Fatalf("a cursor for a changed profile should be rejected: %v", err)
	}
	requireStoreOK(t, a.backlog.Set(fixtureID, firstApp, BacklogEntry{Status: BacklogPlaying}))
	byDimension := startInboxTestHost(t, f, &app{}, time.Now, dir, "en", `"memory_export_enabled":true,"memory_max_items":1,"memory_export_dimensions":["active_games"]`)
	filtered := memoryAnswer(t, byDimension, 500, "")
	if len(filtered.Entries) != 1 || filtered.HasMore || filtered.Entries[0].Facet != "active_games" || !strings.Contains(filtered.Entries[0].Text, "Fixture Quest") {
		t.Fatalf("wrong dimension, order, item limit, or persistent cache: %+v", filtered)
	}
	if steamRequests(f) != requests {
		t.Fatal("reading the on-disk game cache contacted Steam")
	}
	// Assembly can read while the same slots are occupied by foreground requests.
	a.library.gate <- struct{}{}
	defer func() { <-a.library.gate }()
	_ = memoryAnswer(t, h, 500, "")
}

func TestMemoryEntriesTextBudgetBoundsNonASCII(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	var games []game
	for i := uint32(0); i < 100; i++ {
		games = append(games, game{AppID: firstApp + i, Name: strings.Repeat("合成游戏", 32), PlaytimeForever: 2000, Playtime2Weeks: 10})
	}
	raw, _ := json.Marshal(games)
	f.reply(ownedPath, 200, fmt.Sprintf(`{"response":{"games":%s}}`, raw))
	h := startInboxTestHost(t, f, &app{}, time.Now, t.TempDir(), "zh-Hans", `"memory_export_enabled":true,"memory_max_items":100`)
	toolAnswer(t, h, "steam_library", `{}`)
	got := memoryAnswer(t, h, 500, "")
	bytes := 0
	ids := make(map[string]bool)
	for _, e := range got.Entries {
		bytes += len(e.Text)
		if ids[e.ID] || len(e.Subjects) == 0 || !slices.Contains(e.Subjects, "Steam") || e.Status != "active" || e.Rank <= 0 {
			t.Errorf("invalid entry: %+v", e)
		}
		ids[e.ID] = true
	}
	if bytes == 0 || bytes > got.StandingBytes || got.StandingBytes != 2048 || len(got.Entries) >= 100 || got.HasMore {
		t.Fatalf("export budget was bypassed or empty: %d bytes in %d entries", bytes, len(got.Entries))
	}
}

func TestMemoryEntriesQRSessionDoesNotReadKeyCacheAndClearsOnLogout(t *testing.T) {
	dir := t.TempDir()
	keys := newFakeSteam(t, time.Second)
	keys.reply(ownedPath, 200, fmt.Sprintf(`{"response":{"games":[{"appid":%d,"name":"Old key-mode title","playtime_forever":2888}]}}`, firstApp))
	keyHost := startInboxTestHost(t, keys, &app{}, time.Now, dir, "en", `"memory_export_enabled":true`)
	toolAnswer(t, keyHost, "steam_library", `{}`)
	keyFile := filepath.Join(dir, "steam-cache-v1", "library.json")
	keyCache, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	local := NewBacklogStore(dir)
	requireStoreOK(t, local.Set(fixtureID, firstApp, BacklogEntry{Status: BacklogPlaying, Note: "QR test note"}))
	a := &app{}
	f, h := startRecapQRHost(t, a, dir, time.Now, `{"auth_mode":"qr","memory_export_enabled":true}`)
	assertEmptyMemory(t, memoryAnswer(t, h, 500, ""))
	authStep(t, h, "")
	await(t, 5*time.Second, func() bool { return authStep(t, h, qrStep).Done })
	requests := steamRequests(f)
	got := memoryAnswer(t, h, 500, "")
	if len(got.Entries) != 1 || got.Entries[0].Facet != string(memoryBacklog) || steamRequests(f) != requests {
		t.Fatalf("only local annotations should be usable before QR library reads: %+v", got)
	}
	f.reply(progressPath, 200, fmt.Sprintf(`{"response":{"achievement_progress":[{"appid":%d,"unlocked":9,"total":10}]}}`, firstApp))
	toolAnswer(t, h, playtimeRecapName, `{}`)
	toolAnswer(t, h, "steam_achievements", fmt.Sprintf(`{"appid":%d}`, firstApp))
	requests = steamRequests(f)
	got = memoryAnswer(t, h, 500, "")
	raw, _ := json.Marshal(got)
	contains(t, string(raw), "Synthetic Adventure", "9/10")
	assertHidden(t, string(raw), "Old key-mode title", "QR test note", fixtureID)
	if steamRequests(f) != requests {
		t.Fatal("QR memory export contacted Steam")
	}
	a.qr.mu.Lock()
	previous := a.qr.session.reads
	a.qr.mu.Unlock()
	var status wire.AuthStatus
	if err := h.Call("auth.logout", ext.Empty{}, &status); err != nil || status.State != wire.AuthStateSignedOut {
		t.Fatalf("QR logout failed: %s, %v", status.State, err)
	}
	assertEmptyMemory(t, memoryAnswer(t, h, 500, ""))
	requireStoreOK(t, previous.memory.achievements.view(func(entries map[string]observedAchievement) {
		if len(entries) != 0 {
			t.Error("QR sign-out left personal achievements in memory")
		}
	}))
	if got, ok, err := local.Get(fixtureID, firstApp); err != nil || !ok || got.Status != BacklogPlaying {
		t.Fatalf("QR sign-out lost the user's local annotation: %v", err)
	}
	if data, err := os.ReadFile(keyFile); err != nil || string(data) != string(keyCache) {
		t.Fatalf("QR changed an existing key-mode cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, storeDirectory, "playtime-recap.json")); !os.IsNotExist(err) {
		t.Fatalf("QR wrote a playtime recap to disk: %v", err)
	}
}
