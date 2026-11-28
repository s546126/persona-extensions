package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func markAnswer(t *testing.T, h *exttest.Host, appID uint32, fields string) string {
	t.Helper()
	return toolAnswer(t, h, "steam_backlog_mark", fmt.Sprintf(`{"appid":%d,%s}`, appID, fields))
}

func TestBacklogKeyPersistence(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	h := startTestHost(t, f, fixtureVanity, "", dir)
	contains(t, markAnswer(t, h, firstApp, `"status":"queued","note":"Play next","tags":["solo"]`),
		`"available":true`, `"status":"queued"`, `"note":"Play next"`, `"tags":["solo"]`)
	if f.hit(vanityPath) != 1 || f.hit(playerPath) != 0 || f.hit(ownedPath) != 0 || f.hit(recentPath) != 0 {
		t.Fatal("marking a vanity name should only resolve its current SteamID")
	}
	path := filepath.Join(dir, storeDirectory, "backlog.json")
	raw, err := os.ReadFile(path)
	requireStoreOK(t, err)
	contains(t, string(raw), fixtureID+"/"+appIDText(firstApp), `"note":"Play next"`)
	assertHidden(t, string(raw), f.key, fixtureVanity, "Synthetic Adventure")

	other := startTestHost(t, f, fixtureID2, "", dir)
	contains(t, markAnswer(t, other, firstApp, `"status":"dropped"`), `"status":"dropped"`)
	reopened := startTestHost(t, f, fixtureID, "", dir)
	contains(t, markAnswer(t, reopened, firstApp, `"status":"playing"`),
		`"status":"playing"`, `"note":"Play next"`, `"tags":["solo"]`)
	contains(t, markAnswer(t, reopened, firstApp, `"status":"paused","note":"","tags":[]`), `"status":"paused"`)
	if got, ok, err := NewBacklogStore(dir).Get(fixtureID, firstApp); err != nil || !ok || got.Note != "" || len(got.Tags) != 0 {
		t.Fatalf("explicit empty fields did not clear note and tags: %+v, %t, %v", got, ok, err)
	}
	contains(t, markAnswer(t, reopened, firstApp, `"clear":true`), `"available":true`, `"annotation":null`)
	if got, ok, err := NewBacklogStore(dir).Get(fixtureID2, firstApp); err != nil || !ok || got.Status != BacklogDropped {
		t.Fatalf("the other SteamID's annotation changed: %+v, %t, %v", got, ok, err)
	}
	if _, ok, err := NewBacklogStore(dir).Get(fixtureID, firstApp); err != nil || ok {
		t.Fatalf("cleared annotation still exists: %t, %v", ok, err)
	}
	if f.hit(playerPath) != 0 || f.hit(ownedPath) != 0 || f.hit(recentPath) != 0 {
		t.Fatal("numeric SteamID marking contacted Steam")
	}

	// A cached vanity resolution can still point to the former account.
	contains(t, toolAnswer(t, h, "steam_backlog_summary", "{}"), `"available":true`)
	f.reply(vanityPath, http.StatusServiceUnavailable, "synthetic outage")
	contains(t, markAnswer(t, h, firstApp, `"status":"completed"`), `"available":false`, string(problemNetwork))
	f.reply(vanityPath, http.StatusOK, fmt.Sprintf(`{"response":{"success":1,"steamid":%q}}`, fixtureID2))
	contains(t, markAnswer(t, h, firstApp, `"status":"queued"`), `"status":"queued"`)
	if got, ok, err := NewBacklogStore(dir).Get(fixtureID2, firstApp); err != nil || !ok || got.Status != BacklogQueued {
		t.Fatalf("vanity editing did not follow the current SteamID: %+v, %t, %v", got, ok, err)
	}
	if _, ok, err := NewBacklogStore(dir).Get(fixtureID, firstApp); err != nil || ok {
		t.Fatalf("vanity editing wrote to a stale SteamID: %t, %v", ok, err)
	}
}

func seedBacklogGames(t *testing.T, f *fakeSteam, owned, recent []game, ownedCount int) {
	t.Helper()
	for _, fixture := range []struct {
		path, countKey string
		count          int
		games          []game
	}{
		{ownedPath, "game_count", ownedCount, owned},
		{recentPath, "total_count", len(recent), recent},
	} {
		raw, err := json.Marshal(map[string]any{"response": map[string]any{
			fixture.countKey: fixture.count,
			"games":          fixture.games,
		}})
		if err != nil {
			t.Fatal(err)
		}
		f.reply(fixture.path, http.StatusOK, string(raw))
	}
}

func TestBacklogSummaryBucketsAndPagination(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	seedBacklogGames(t, f, []game{
		{AppID: firstApp, Name: "A Unopened"},
		{AppID: firstApp + 1, Name: "B One Minute", PlaytimeForever: 1},
		{AppID: firstApp + 2, Name: "C Almost Two Hours", PlaytimeForever: 119},
		{AppID: firstApp + 3, Name: "D Two Hours", PlaytimeForever: 120},
		{AppID: firstApp + 4, Name: "E Just Under Twenty Hours", PlaytimeForever: 1199},
		{AppID: firstApp + 5, Name: "F Twenty Hours", PlaytimeForever: 1200},
		{AppID: firstApp + 6, Name: "G Finished"},
		{AppID: firstApp + 7, Name: "H Dropped"},
		{AppID: firstApp + 8, Name: "I Unopened"},
	}, []game{
		{AppID: firstApp + 2, Playtime2Weeks: 10},
		{AppID: firstApp + 5, Playtime2Weeks: 55},
		{AppID: firstApp + 9, Playtime2Weeks: 90}, // Activity outside the owned library.
	}, 11)
	dir := t.TempDir()
	store := NewBacklogStore(dir)
	for i, status := range map[uint32]BacklogStatus{
		0: BacklogQueued, 2: BacklogPaused, 3: BacklogUnplayed, 4: BacklogPlaying,
		6: BacklogCompleted, 7: BacklogDropped, 9: BacklogPlaying,
	} {
		requireStoreOK(t, store.Set(fixtureID, firstApp+i, BacklogEntry{Status: status}))
	}
	h := startTestHost(t, f, fixtureID, "", dir)
	raw := toolAnswer(t, h, "steam_backlog_summary", "{}")
	var summary struct {
		Available  bool
		Counts     map[string]int
		Games      json.RawMessage
		OwnedCount int `json:"owned_game_count"`
		Analyzed   int `json:"analyzed_game_count"`
		Truncated  bool
	}
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"never_played": 2, "tried_under_2h": 2, "playing": 3, "long_term": 1,
		"queued": 1, "paused": 1, "completed": 1, "dropped": 1,
	}
	if !summary.Available || !reflect.DeepEqual(summary.Counts, want) || summary.Games != nil ||
		summary.OwnedCount != 11 || summary.Analyzed != 9 || !summary.Truncated {
		t.Fatalf("unexpected counts-only summary: %s", raw)
	}

	contains(t, toolAnswer(t, h, "steam_backlog_summary", `{"bucket":"playing","limit":1,"offset":1}`),
		`"matched_count":3`, `"next_offset":2`, "E Just Under Twenty Hours")
	contains(t, toolAnswer(t, h, "steam_backlog_summary", `{"status":"playing"}`),
		`"matched_count":1`, "E Just Under Twenty Hours")
	contains(t, toolAnswer(t, h, "steam_backlog_summary", `{"status":"unplayed"}`),
		`"matched_count":1`, "D Two Hours")
	contains(t, toolAnswer(t, h, "steam_backlog_summary", `{"bucket":"tried_under_2h","status":"paused"}`),
		`"matched_count":1`, "C Almost Two Hours", `"playtime_2weeks":10`, `"status":"paused"`)
	contains(t, toolAnswer(t, h, "steam_backlog_summary", `{"bucket":"long_term","offset":20000}`),
		`"matched_count":1`, `"games":[]`)
}

func TestBacklogQRPersistenceAndLogout(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	saved := fixtureSecret(t, fixtureID, now)
	access := fixtureJWT(t, fixtureID, now.Add(time.Hour), "backlog-access")
	f := newFakeSteamMode(t, time.Second, access)
	f.handle(authBasePath+beginQRPath+"/v1/", func(w http.ResponseWriter, _ *http.Request) {
		authFixture(w, "1", beginFixture())
	})
	f.handle(authBasePath+pollQRPath+"/v1/", func(w http.ResponseWriter, _ *http.Request) {
		authFixture(w, "1", grantFixture(saved.RefreshToken, access))
	})
	dir := t.TempDir()
	t.Setenv("PERSONA_EXTENSION_DATA_DIR", dir)
	a := &app{}
	h := exttest.StartWith(t, manifest, exttest.Serve(t, a.program(f.http, func() time.Time { return now })), func(p *wire.InitializeParams) {
		p.Config = json.RawMessage(fmt.Sprintf(`{"auth_mode":"qr","steam_id":%q}`, fixtureID2))
		p.Hub.Locale = "zh-Hans"
	})
	contains(t, markAnswer(t, h, firstApp, `"status":"playing"`), `"available":false`, string(problemAuthRequired))
	authStep(t, h, "")
	await(t, 5*time.Second, func() bool { return authStep(t, h, qrStep).Done })

	contains(t, toolAnswer(t, h, "steam_backlog_summary", "{}"), `"available":true`, `"owned_game_count":3`)
	picks := toolAnswer(t, h, "steam_tonight_pick", `{"limit":1}`)
	contains(t, picks, "你在近两周玩过这款游戏。")
	for _, name := range []string{"steam-cache-v1", storeDirectory} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("QR reads created %s on disk: %v", name, err)
		}
	}
	contains(t, markAnswer(t, h, firstApp, `"status":"playing","note":"本机测试","tags":["短局"]`),
		`"available":true`, `"note":"本机测试"`, `"tags":["短局"]`)
	contains(t, toolAnswer(t, h, "steam_backlog_summary", `{"status":"playing"}`), `"matched_count":1`, "Synthetic Adventure")
	if f.hit(ownedPath) != 1 || f.hit(recentPath) != 1 || f.hit(playerLinkPath) != 0 {
		t.Fatal("QR backlog operations should reuse the library and recent reads for the signed-in SteamID")
	}
	contents, err := os.ReadDir(filepath.Join(dir, storeDirectory))
	requireStoreOK(t, err)
	for _, file := range contents {
		if file.Name() != "backlog.json" && file.Name() != "backlog.json.lock" {
			t.Errorf("QR mode persisted a remote snapshot: %s", file.Name())
		}
	}
	path := filepath.Join(dir, storeDirectory, "backlog.json")
	raw, err := os.ReadFile(path)
	requireStoreOK(t, err)
	contains(t, string(raw), fixtureID+"/"+appIDText(firstApp))
	assertHidden(t, string(raw), access, saved.RefreshToken, fixtureID2, "Synthetic Adventure", "playtime")

	var status wire.AuthStatus
	if err := h.Call("auth.logout", struct{}{}, &status); err != nil || status.State != wire.AuthStateSignedOut {
		t.Fatalf("QR logout failed: %v, %q", err, status.State)
	}
	contains(t, markAnswer(t, h, firstApp, `"clear":true`), `"available":false`, string(problemAuthRequired))
	contains(t, toolAnswer(t, h, "steam_backlog_summary", "{}"), `"available":false`, string(problemAuthRequired))
	if got, err := os.ReadFile(path); err != nil || string(got) != string(raw) {
		t.Fatal("QR sign-out or a signed-out request modified the local annotations")
	}
	other := fixtureSecret(t, fixtureID2, now)
	a.qr.mu.Lock()
	a.qr.installLocked(steamTokens{secret: other, refreshExpiry: now.Add(time.Hour)})
	a.qr.mu.Unlock()
	result := markAnswer(t, h, firstApp, `"status":"queued"`)
	contains(t, result, `"status":"queued"`)
	assertHidden(t, result, "本机测试", "短局")
	if got, ok, err := NewBacklogStore(dir).Get(fixtureID, firstApp); err != nil || !ok ||
		got.Status != BacklogPlaying || got.Note != "本机测试" || !reflect.DeepEqual(got.Tags, []string{"短局"}) {
		t.Fatalf("the original QR account's annotation did not survive logout and switching accounts: %+v, %t, %v", got, ok, err)
	}
}

func TestTonightPicksUseMoodTimeAndFilters(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	seedBacklogGames(t, f, []game{
		{AppID: firstApp, Name: "Campaign", PlaytimeForever: 3000},
		{AppID: firstApp + 1, Name: "Arcade", PlaytimeForever: 75},
		{AppID: firstApp + 2, Name: "Queued"},
		{AppID: firstApp + 3, Name: "Hidden Café", PlaytimeForever: 45},
		{AppID: firstApp + 4, Name: "Completed", PlaytimeForever: 150},
		{AppID: firstApp + 5, Name: "Dropped"},
		{AppID: firstApp + 6, Name: "Unopened"},
		{AppID: firstApp + 7, Name: "Long Session", PlaytimeForever: 230},
	}, []game{
		{AppID: firstApp, Playtime2Weeks: 110},
		{AppID: firstApp + 1, Playtime2Weeks: 20},
		{AppID: firstApp + 4, Playtime2Weeks: 5},
		{AppID: firstApp + 7, Playtime2Weeks: 65},
	}, 8)
	dir := t.TempDir()
	store := NewBacklogStore(dir)
	for i, entry := range map[uint32]BacklogEntry{
		0: {Status: BacklogPlaying, Note: "Private campaign note", Tags: []string{"Solo"}},
		2: {Status: BacklogQueued},
		3: {Status: BacklogPaused, Tags: []string{" Solo "}},
		4: {Status: BacklogCompleted, Tags: []string{"Solo"}},
		5: {Status: BacklogDropped, Tags: []string{"Solo"}},
	} {
		requireStoreOK(t, store.Set(fixtureID, firstApp+i, entry))
	}
	h := startTestHost(t, f, fixtureID, "", dir)
	for _, tc := range []struct{ args, first, reason string }{
		{`{"available_minutes":30,"mood":"quick_session","limit":1}`, "Arcade", english.backlog.pickTime},
		{`{"available_minutes":120,"mood":"quick_session","limit":1}`, "Campaign", english.backlog.pickTime},
		{`{"mood":"continue_playing","limit":1}`, "Campaign", english.backlog.pickPlaying},
		{`{"mood":"clear_backlog","limit":1}`, "Queued", english.backlog.pickQueued},
		{`{"mood":"hidden_gem","limit":1}`, "Hidden Café", english.backlog.pickTried},
		{`{"mood":"any","tag":" solo ","query":"cAFÉ","limit":1}`, "Hidden Café", english.backlog.pickTried},
		{`{"include_completed":true,"query":"Completed","limit":1}`, "Completed", english.backlog.pickCompleted},
	} {
		raw := toolAnswer(t, h, "steam_tonight_pick", tc.args)
		var reply struct {
			Available bool
			Picks     []tonightPick
		}
		if err := json.Unmarshal([]byte(raw), &reply); err != nil || !reply.Available || len(reply.Picks) != 1 ||
			reply.Picks[0].Name != tc.first || reply.Picks[0].Reason != tc.reason {
			t.Errorf("args %s gave %s, want %s with reason %q", tc.args, raw, tc.first, tc.reason)
		}
		assertHidden(t, raw, "Private campaign note", `"tags"`, `"note"`, `"score"`)
	}
	raw := toolAnswer(t, h, "steam_tonight_pick", "{}")
	var defaults struct {
		AvailableMinutes int `json:"available_minutes"`
		Mood             string
		Picks            []tonightPick
	}
	if err := json.Unmarshal([]byte(raw), &defaults); err != nil || len(defaults.Picks) != 5 ||
		defaults.Mood != "any" || defaults.AvailableMinutes != 60 {
		t.Fatalf("unexpected recommendation defaults: %s", raw)
	}
	assertHidden(t, raw, "Completed", "Dropped")
	contains(t, toolAnswer(t, h, "steam_tonight_pick", `{"include_completed":true,"query":"Dropped"}`), `"picks":[]`)
}

func TestBacklogRejectsInvalidArgumentsBeforeRequestsOrWrites(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	h := startTestHost(t, f, fixtureVanity, "", dir)
	for _, tc := range []struct{ name, args string }{
		{"steam_backlog_summary", `{"bucket":"discarded"}`},
		{"steam_backlog_summary", `{"status":"never_played"}`},
		{"steam_backlog_summary", `{"offset":20001}`},
		{"steam_backlog_summary", `{"limit":51}`},
		{"steam_backlog_mark", `{"appid":0,"status":"playing"}`},
		{"steam_backlog_mark", `{"appid":4294967296,"status":"playing"}`},
		{"steam_backlog_mark", `{"appid":42}`},
		{"steam_backlog_mark", `{"appid":42,"status":"unknown"}`},
		{"steam_backlog_mark", `{"appid":42,"status":"playing","clear":true}`},
		{"steam_backlog_mark", `{"appid":42,"note":"x","clear":true}`},
		{"steam_backlog_mark", `{"appid":42,"status":"playing","tags":[" "]}`},
		{"steam_backlog_mark", `{"appid":42,"status":"playing","tags":["a","b","c","d","e","f","g","h","i"]}`},
		{"steam_backlog_mark", fmt.Sprintf(`{"appid":42,"status":"queued","note":%q}`, strings.Repeat("测", 241))},
		{"steam_backlog_mark", fmt.Sprintf(`{"appid":42,"status":"queued","tags":[%q]}`, strings.Repeat("测", 33))},
		{"steam_backlog_mark", fmt.Sprintf(`{"appid":42,"status":"playing","steam_id":%q}`, fixtureID2)},
		{"steam_tonight_pick", `{"mood":"coop"}`},
		{"steam_tonight_pick", `{"available_minutes":0}`},
		{"steam_tonight_pick", `{"available_minutes":1441}`},
		{"steam_tonight_pick", `{"limit":21}`},
		{"steam_tonight_pick", `{"tag":" "}`},
		{"steam_tonight_pick", fmt.Sprintf(`{"query":%q}`, strings.Repeat("测", 101))},
		{"steam_tonight_pick", `{"include_completed":"yes"}`},
	} {
		var e *ext.Error
		err := h.Call("connector.tools.call", wire.ConnectorCallParams{Contribution: extensionID, Name: tc.name, Args: json.RawMessage(tc.args)}, nil)
		if !errors.As(err, &e) || e.Code != "invalid" {
			t.Errorf("%s accepted %s: %v", tc.name, tc.args, err)
		}
	}
	if f.hit(vanityPath) != 0 || f.hit(playerPath) != 0 || f.hit(ownedPath) != 0 || f.hit(recentPath) != 0 {
		t.Fatal("invalid backlog input triggered an upstream request")
	}
	if _, err := os.Stat(filepath.Join(dir, storeDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid input created local storage: %v", err)
	}
}

func TestBacklogViewStatusActions(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	f.handle(newsForAppPath, func(w http.ResponseWriter, r *http.Request) {
		app := r.URL.Query().Get("appid")
		_, _ = fmt.Fprintf(w, `{"appnews":{"appid":%s,"newsitems":[
			{"gid":%q,"date":1720000000,"title":"Fixture Patch","feedname":"steam_community_announcements"}]}}`, app, app)
	})
	dir := t.TempDir()
	h := startTestHost(t, f, fixtureID, "", dir)
	nav := wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, UIVersion: 12, Locale: "zh-Hans"}
	overview := viewAnswer(t, h, nav)
	markAnswer(t, h, firstApp, `"status":"queued","note":"合成备注","tags":["单人"]`)
	overview = act(t, h, nav, overview, "refresh", nil)
	news := act(t, h, nav, overview, gameNewsAction, nil)
	if news.Title != "近期官方更新" || len(news.Sections[0].Items) < 2 || news.Sections[0].Items[1].Type != wire.ComponentLink {
		t.Fatalf("the news page did not expose official update links: %+v", news)
	}
	overview = act(t, h, nav, news, "back", nil)
	raw, err := json.Marshal(overview)
	requireStoreOK(t, err)
	contains(t, string(raw), "吃灰盘点与标注", "从未打开", "试水不足 2 小时", "游玩至少 20 小时",
		`{"label":"待玩","value":"1"}`, `"badge":{"text":"待玩"}`)

	detail := act(t, h, nav, overview, "library-game-"+appIDText(firstApp), nil)
	raw, err = json.Marshal(detail)
	requireStoreOK(t, err)
	contains(t, string(raw), "本机游戏标注", "合成备注", "单人", `"id":"backlog-save","submit":true`,
		`"id":"backlog-clear","confirm":`)
	detail = act(t, h, nav, detail, "backlog-save", map[string]string{"backlog_status": "completed"})
	raw, err = json.Marshal(detail)
	requireStoreOK(t, err)
	contains(t, string(raw), `"value":"completed"`)
	if got, ok, err := NewBacklogStore(dir).Get(fixtureID, firstApp); err != nil || !ok ||
		got.Status != BacklogCompleted || got.Note != "合成备注" || !reflect.DeepEqual(got.Tags, []string{"单人"}) {
		t.Fatalf("UI status changed or lost the wrong fields: %+v, %t, %v", got, ok, err)
	}

	other := startTestHost(t, f, fixtureID2, "", dir)
	staleAction := nav
	staleAction.Action, staleAction.State = "backlog-clear", detail.State
	var e *ext.Error
	if err := other.Call("ui.action", staleAction, nil); !errors.As(err, &e) || e.Code != "invalid" {
		t.Fatalf("a detail action from the previous SteamID was accepted: %v", err)
	}

	detail = act(t, h, nav, detail, "backlog-clear", nil)
	raw, err = json.Marshal(detail)
	requireStoreOK(t, err)
	contains(t, string(raw), "暂无本机标注")
	assertHidden(t, string(raw), "合成备注", "单人", "backlog-clear")
	contains(t, toolAnswer(t, h, "steam_backlog_summary", `{"status":"completed"}`), `"games":[]`)
}
