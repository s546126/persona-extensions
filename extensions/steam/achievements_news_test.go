package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func seedAchievementRanking(t *testing.T, f *fakeSteam) {
	f.handle("/ISteamUserStats/GetGlobalAchievementPercentagesForApp/v2/", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); len(q) != 1 || q.Get("gameid") != appIDText(firstApp) {
			t.Error("global rates must be requested anonymously by gameid")
		}
		_, _ = io.WriteString(w, `{"achievementpercentages":{"achievements":[
			{"name":"hard","percent":"2.5"},{"name":"old","percent":95},{"name":"secret_ending","percent":"40.2"},
			{"name":"easy","percent":"80.1"},{"name":"new","percent":90}]}}`)
	})
}

func TestAchievementHunterRanksLockedAndDatedUnlocks(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	seedAchievementRanking(t, f)
	f.reply(achievementsPath, http.StatusOK, `{"playerstats":{"success":true,"gameName":"Synthetic Adventure","achievements":[
		{"apiname":"hard","achieved":0},{"apiname":"old","achieved":1,"unlocktime":100},
		{"apiname":"new","achieved":1,"unlocktime":200},{"apiname":"easy","achieved":0},
		{"apiname":"secret_ending","achieved":0}]}}`)
	f.reply("/ISteamUserStats/GetSchemaForGame/v2/", http.StatusOK, `{"game":{"gameName":"Synthetic Adventure","availableGameStats":{"achievements":[
		{"name":"hard","displayName":"Steep Climb","hidden":0},{"name":"old","displayName":"First Win","hidden":0},
		{"name":"new","displayName":"Second Win","hidden":0},{"name":"easy","displayName":"Getting Started","hidden":0,"description":"Finish the tutorial"},
		{"name":"secret_ending","displayName":"Ending Spoiler","hidden":1,"description":"Spoiler text"}]}}}`)
	h := startTestHost(t, f, fixtureID, "GB", "")
	answer := toolAnswer(t, h, "steam_achievement_hunter", fmt.Sprintf(`{"appid":%d,"limit":2}`, firstApp))
	var got struct {
		Available       bool `json:"available"`
		PersonalDetails bool `json:"personal_details"`
		NextOffset      int  `json:"next_offset"`
		Progress        achievementProgress
		Locked          []struct {
			ID      string  `json:"api_name"`
			Hidden  *bool   `json:"hidden"`
			Percent float64 `json:"global_percent"`
		}
		Timeline []struct {
			Time int64 `json:"unlock_time"`
		}
	}
	if err := json.Unmarshal([]byte(answer), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Available || !got.PersonalDetails || got.Progress.Total != 5 || got.Progress.Unlocked != 2 ||
		len(got.Locked) != 2 || got.Locked[0].ID != "easy" || got.Locked[0].Percent != 80.1 || got.Locked[0].Hidden == nil || *got.Locked[0].Hidden ||
		got.Locked[1].Hidden == nil || !*got.Locked[1].Hidden || got.Locked[1].Percent != 40.2 || got.NextOffset != 2 {
		t.Fatalf("incorrect achievement sprint: %s", answer)
	}
	if len(got.Timeline) != 2 || got.Timeline[0].Time != 200 || got.Timeline[1].Time != 100 {
		t.Fatalf("incorrect unlock timeline: %s", answer)
	}
	assertHidden(t, answer, "secret_ending", "Ending Spoiler", "Spoiler text")
	page := toolAnswer(t, h, "steam_achievement_hunter", fmt.Sprintf(`{"appid":%d,"limit":2,"offset":2}`, firstApp))
	contains(t, page, `"api_name":"hard"`, `"timeline":[]`)
	if hits := f.hit("/ISteamUserStats/GetGlobalAchievementPercentagesForApp/v2/"); hits != 1 {
		t.Fatalf("pagination used %d global requests, want 1", hits)
	}
}

func TestAchievementHunterPrivateProfileUsesGlobalRankings(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	seedAchievementRanking(t, f)
	f.reply(playerPath, http.StatusOK, fmt.Sprintf(`{"response":{"players":[{"steamid":%q,"communityvisibilitystate":1}]}}`, fixtureID))
	f.reply("/ISteamUserStats/GetSchemaForGame/v2/", http.StatusOK, `{"game":{"availableGameStats":{"achievements":[
		{"name":"old","displayName":"First Win","hidden":0},{"name":"hard","displayName":"Steep Climb","hidden":0},
		{"name":"secret_ending","displayName":"Ending Spoiler","hidden":1}]}}}`)
	h := startTestHost(t, f, fixtureID, "GB", "")
	answer := toolAnswer(t, h, "steam_achievement_hunter", fmt.Sprintf(`{"appid":%d,"limit":1}`, firstApp))
	contains(t, answer, `"available":true`, `"personal_details":false`, `"code":"private_profile"`, `"unlocked":null`, `"total":5`,
		`"global_easiest":[{"api_name":"old"`, `"global_rarest":[{"api_name":"hard"`)
	assertHidden(t, answer, "secret_ending", "Ending Spoiler", `"locked":`, `"timeline":`)
	if f.hit(achievementsPath) != 0 {
		t.Fatal("the hunter attempted to read achievements for a private Key-mode profile")
	}
}

func TestAchievementHunterQRDetailsAndCountOnly(t *testing.T) {
	now := time.Now()
	access := fixtureJWT(t, fixtureID, now.Add(time.Hour), "hunter-access")
	saved := fixtureSecret(t, fixtureID, now)
	f := newFakeSteamMode(t, time.Second, access)
	f.handle(authBasePath+beginQRPath+"/v1/", func(w http.ResponseWriter, _ *http.Request) {
		authFixture(w, "1", beginFixture())
	})
	f.handle(authBasePath+pollQRPath+"/v1/", func(w http.ResponseWriter, _ *http.Request) {
		authFixture(w, "1", grantFixture(saved.RefreshToken, access))
	})
	seedAchievementRanking(t, f)
	f.handle(gameAchievementsPath, func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("appid") != appIDText(firstApp) || q.Get("language") != "english" {
			t.Error("the public schema did not receive the requested app and language")
		}
		_, _ = io.WriteString(w, `{"response":{"schema_hash":15,"achievements":[
			{"internal_name":"easy","internal_key":81,"localized_name":"Getting Started"},
			{"internal_name":"hard","internal_key":82,"localized_name":"Steep Climb"},
			{"internal_name":"old","internal_key":83,"localized_name":"First Win"},
			{"internal_name":"new","internal_key":84,"localized_name":"Second Win"},
			{"internal_name":"secret_ending","internal_key":85,"localized_name":"Ending Spoiler","hidden":true},
			{"internal_name":"retired","internal_key":86,"archived":true}]}}`)
	})
	f.handle(userAchievementsPath, func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("appid") != appIDText(firstApp) || q.Get("steamid") != fixtureID {
			t.Error("the QR detail call did not target the signed-in account and requested app")
		}
		_, _ = io.WriteString(w, `{"response":{"schema_hash":15,"achievements":[
			{"internal_key":84,"unlocked":true,"unlock_time":200},{"internal_key":83,"unlocked":true,"unlock_time":100},
			{"internal_key":81,"unlocked":false}]}}`)
	})
	f.reply(progressPath, http.StatusOK, fmt.Sprintf(`{"response":{"achievement_progress":[{"appid":%d,"unlocked":2,"total":5}]}}`, firstApp))
	h := exttest.StartWith(t, manifest, exttest.Serve(t, program(f.http, func() time.Time { return now })), func(p *wire.InitializeParams) {
		p.Config = json.RawMessage(`{"auth_mode":"qr"}`)
		p.Hub.Locale = "en"
	})
	authStep(t, h, "")
	await(t, 5*time.Second, func() bool { return authStep(t, h, qrStep).Done })
	args := fmt.Sprintf(`{"appid":%d}`, firstApp)
	answer := toolAnswer(t, h, achievementHunterName, args)
	contains(t, answer, `"personal_details":true`, `"limit":20`, `"timeline":[{"api_name":"new"`,
		`"locked":[{"api_name":"easy"`, `"api_name":"hard"`, `"hidden":true`)
	assertHidden(t, answer, "retired", "secret_ending", "Ending Spoiler", access, saved.RefreshToken)

	f.reply(userAchievementsPath, http.StatusOK, `{"response":{"schema_hash":15,"achievements":[]}}`)
	f.reply(progressPath, http.StatusOK, fmt.Sprintf(`{"response":{"achievement_progress":[{"appid":%d,"unlocked":0,"total":5}]}}`, firstApp))
	answer = toolAnswer(t, h, achievementHunterName, args)
	var zero struct {
		PersonalDetails bool `json:"personal_details"`
		Progress        achievementProgress
		Locked          []rankedAchievement
		Timeline        []rankedAchievement
	}
	if err := json.Unmarshal([]byte(answer), &zero); err != nil {
		t.Fatal(err)
	}
	if !zero.PersonalDetails || zero.Progress.Unlocked != 0 || zero.Progress.Total != 5 ||
		len(zero.Locked) != 5 || zero.Timeline == nil || len(zero.Timeline) != 0 {
		t.Fatalf("zero unlocks should show the full locked roster and an empty timeline: %s", answer)
	}
	assertHidden(t, answer, "retired", "secret_ending", "Ending Spoiler", access, saved.RefreshToken)

	f.reply(userAchievementsPath, http.StatusForbidden, "private")
	f.reply(progressPath, http.StatusOK, fmt.Sprintf(`{"response":{"achievement_progress":[{"appid":%d,"unlocked":2,"total":5}]}}`, firstApp))
	answer = toolAnswer(t, h, achievementHunterName, args)
	contains(t, answer, `"personal_details":false`, `"total":5`, `"unlocked":2`, string(problemAchievementDetails),
		`"global_easiest":[{"api_name":"old"`, `"global_rarest":[{"api_name":"hard"`)
	assertHidden(t, answer, `"locked":`, `"timeline":`, "secret_ending", access, saved.RefreshToken)
	if f.hit(playerLinkPath) != 0 || f.hit(achievementsPath) != 0 || f.hit(globalAchievementsPath) != 1 {
		t.Fatal("QR achievement calls should use the signed-in SteamID and reuse cached public rates")
	}
}

func TestGlobalAchievementRequestRejectsRedirectsAndOversizedBodies(t *testing.T) {
	for _, name := range []string{"redirect", "oversized"} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSteam(t, 2*time.Second)
			f.handle(globalAchievementsPath, func(w http.ResponseWriter, r *http.Request) {
				if name == "redirect" {
					http.Redirect(w, r, apiBase+globalAchievementsPath+"?gameid="+appIDText(secondApp), http.StatusFound)
					return
				}
				_, _ = io.WriteString(w, `{"achievementpercentages":{"achievements":[{"name":"easy","percent":85}]}}`)
				_, _ = io.WriteString(w, strings.Repeat(" ", maxAPIBytes))
			})
			c := steamClient{http: f.http}
			if data, issue := c.globalAchievementRates(t.Context(), firstApp); data != nil || issue != problemResponse || f.hit(globalAchievementsPath) != 1 {
				t.Fatalf("the %s global request should fail without following a redirect: %v, %s", name, data, issue)
			}
		})
	}
}

func TestGameNewsOfficialFilteringAndMetadataOnly(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	f.handle("/ISteamNews/GetNewsForApp/v2/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("appid") != appIDText(firstApp) || q.Get("maxlength") != "1" || q.Get("count") == "" {
			t.Error("metadata-only news did not ask Steam for the shortest blurb")
		}
		if feeds := q.Get("feeds"); feeds != "steam_community_announcements" && feeds != "" {
			t.Error("news used an unexpected feed")
		}
		_, _ = fmt.Fprintf(w, `{"appnews":{"appid":%d,"newsitems":[
			{"gid":"101","date":400,"title":"Patch 2 review","feedname":"Fixture Press","feedlabel":"Fixture Press",
			 "url":"https://example.com/review","contents":"FULL_TEXT_SENTINEL"},
			{"gid":"102","date":350,"title":"Community Update: Fan Art Roundup","feedname":"steam_community_announcements","contents":"FULL_TEXT_SENTINEL"},
			{"gid":"103","date":220,"title":"A New Campaign","tags":["dlc"],"feedname":"steam_community_announcements","contents":"FULL_TEXT_SENTINEL"},
			{"gid":"104","date":300,"title":"Major Update 2.0","feedname":"steam_community_announcements","feedlabel":"Community Announcements",
			 "is_external_url":true,"url":"https://steamstore-a.akamaihd.net/news/externalpost/steam_community_announcements/104",
			 "contents":{"unexpected":"FULL_TEXT_SENTINEL"}},
			{"gid":"104","date":200,"title":"Duplicate Update","feedname":"steam_community_announcements","contents":"FULL_TEXT_SENTINEL"},
			{"gid":"105","date":250,"title":"Hotfix","tags":["patchnotes"],"feedname":"steam_community_announcements","contents":"FULL_TEXT_SENTINEL"}]}}`, firstApp)
	})
	h := startTestHost(t, f, fixtureID, "GB", "")
	assertMetadata := func(answer string, wantGIDs []string) {
		t.Helper()
		var got struct {
			Available      bool                         `json:"available"`
			SummaryEnabled *bool                        `json:"summary_enabled"`
			Updates        []map[string]json.RawMessage `json:"updates"`
		}
		if err := json.Unmarshal([]byte(answer), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Available || got.SummaryEnabled == nil || *got.SummaryEnabled || len(got.Updates) != len(wantGIDs) {
			t.Fatalf("incorrect metadata-only response: %s", answer)
		}
		for i, item := range got.Updates {
			if len(item) != 7 || string(item["gid"]) != fmt.Sprintf("%q", wantGIDs[i]) {
				t.Fatalf("incorrect metadata or order at index %d: %v", i, item)
			}
			for _, field := range []string{"appid", "game_name", "gid", "title", "date", "feed_label", "url"} {
				if _, ok := item[field]; !ok {
					t.Errorf("missing required metadata field %s", field)
				}
			}
		}
		assertHidden(t, answer, "FULL_TEXT_SENTINEL", "Duplicate Update", `"excerpt":`, `"contents":`)
	}
	official := toolAnswer(t, h, "steam_game_news", fmt.Sprintf(`{"appid":%d}`, firstApp))
	assertMetadata(official, []string{"104", "105", "103"})
	contains(t, official, "Synthetic Adventure", "Community Announcements", storeBase+"/news/app/"+appIDText(firstApp)+"/view/104")
	assertHidden(t, official, "Fixture Press", "Fan Art Roundup")
	mixed := toolAnswer(t, h, "steam_game_news", fmt.Sprintf(`{"appid":%d,"official_only":false}`, firstApp))
	assertMetadata(mixed, []string{"101", "102", "104", "105", "103"})
	contains(t, mixed, "https://example.com/review", "Fixture Press")
	if f.hit(playerPath) != 0 {
		t.Fatal("an explicit AppID news request looked up the user's account")
	}
}

func TestGameNewsSummaryCleansAndBoundsExcerpts(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	const visible = "Update & fixes"
	markup := `[h2]Update &amp; fixes[/h2]<p>[url=https://example.com/noise]Read more[/url]</p>
<script>scriptNoise</script><!-- commentNoise -->[img]{STEAM_CLAN_IMAGE}/imageNoise.jpg[/img]
[previewyoutube=example;full][/previewyoutube][list][*][b]修复[/b][/list] ` + strings.Repeat("更", 900) + "tooLong"
	f.handle("/ISteamNews/GetNewsForApp/v2/", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("feeds") != "steam_community_announcements" || q.Get("maxlength") == "" || q.Get("maxlength") == "0" || q.Get("maxlength") == "1" {
			t.Error("summary mode did not request a bounded official blurb")
		}
		_, _ = fmt.Fprintf(w, `{"appnews":{"appid":%d,"newsitems":[`, firstApp)
		for i := range 23 {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			encoded, _ := json.Marshal(markup)
			_, _ = fmt.Fprintf(w, `{"gid":"%d","date":%d,"title":"Patch Notes","feedname":"steam_community_announcements","contents":%s}`, 100+i, 200+i, encoded)
		}
		_, _ = io.WriteString(w, "]}}")
	})
	h := exttest.StartWith(t, manifest, exttest.Serve(t, program(f.http, time.Now)), func(p *wire.InitializeParams) {
		p.Config = json.RawMessage(`{"auth_mode":"qr","patch_notes_summary_enabled":true}`)
		p.Hub.Locale = "en"
	})
	for _, limit := range []int{10, 20} {
		args := fmt.Sprintf(`{"appid":%d,"limit":%d}`, firstApp, limit)
		if limit == 10 {
			args = fmt.Sprintf(`{"appid":%d}`, firstApp)
		}
		answer := toolAnswer(t, h, "steam_game_news", args)
		var got struct {
			SummaryEnabled bool `json:"summary_enabled"`
			Updates        []struct {
				Excerpt string
				Date    int64
			}
		}
		if err := json.Unmarshal([]byte(answer), &got); err != nil {
			t.Fatal(err)
		}
		if !got.SummaryEnabled || len(got.Updates) != limit || got.Updates[0].Date != 222 {
			t.Fatalf("summary mode or limits were incorrect: %t, %d items", got.SummaryEnabled, len(got.Updates))
		}
		for _, item := range got.Updates {
			count := utf8.RuneCountInString(item.Excerpt)
			if !strings.HasPrefix(item.Excerpt, visible+" Read more 修复 ") || !utf8.ValidString(item.Excerpt) || count < 600 || count > 800 {
				t.Fatalf("excerpt was not cleaned and rune-bounded: %q", item.Excerpt)
			}
			assertHidden(t, item.Excerpt, "scriptNoise", "commentNoise", "imageNoise", "tooLong", "example.com", "[", "<")
		}
	}
	if f.hit("/ISteamNews/GetNewsForApp/v2/") != 1 || f.hit(storeNamesPath) != 1 {
		t.Fatal("repeated reads should reuse cached articles and the Store name")
	}
}

func TestGameNewsRecentGamesAndLibraryFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("library_fallback_%t", fallback), func(t *testing.T) {
			f := newFakeSteam(t, time.Second)
			if fallback {
				f.reply(recentPath, http.StatusOK, `{"response":{"total_count":0}}`)
			}
			f.handle("/ISteamNews/GetNewsForApp/v2/", func(w http.ResponseWriter, r *http.Request) {
				rawID := r.URL.Query().Get("appid")
				if feeds := r.URL.Query().Get("feeds"); feeds != "steam_community_announcements" {
					t.Error("automatic news did not request the official feed")
				}
				dates := map[string]int{appIDText(firstApp): 200, appIDText(secondApp): 300, appIDText(thirdApp): 400}
				if dates[rawID] == 0 {
					t.Error("news queried an unexpected AppID")
				}
				_, _ = fmt.Fprintf(w, `{"appnews":{"appid":%s,"newsitems":[{"gid":%q,"date":%d,"title":"Patch Notes","feedname":"steam_community_announcements"}]}}`,
					rawID, rawID, dates[rawID])
			})
			h := startTestHost(t, f, fixtureID, "GB", "")
			answer := toolAnswer(t, h, "steam_game_news", `{"games_limit":2,"limit":2}`)
			var got struct {
				Updates []struct {
					AppID uint32
				}
			}
			if err := json.Unmarshal([]byte(answer), &got); err != nil {
				t.Fatal(err)
			}
			var ids []uint32
			for _, item := range got.Updates {
				ids = append(ids, item.AppID)
			}
			want, ownedReads := []uint32{secondApp, firstApp}, 0
			if fallback {
				want, ownedReads = []uint32{thirdApp, secondApp}, 1
			}
			if !slices.Equal(ids, want) || f.hit(ownedPath) != ownedReads || f.hit("/ISteamNews/GetNewsForApp/v2/") != 2 {
				t.Fatalf("news should merge two selected games in date order: got %v, want %v", ids, want)
			}
		})
	}
}

func TestRecentOfficialUpdatesPageLoadsOnDemandAndReusesNews(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	f.handle(newsForAppPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("maxlength") != "1" || q.Get("feeds") != officialNewsFeed {
			t.Error("the news page should only request official headlines")
		}
		app := q.Get("appid")
		_, _ = fmt.Fprintf(w, `{"appnews":{"appid":%s,"newsitems":[
			{"gid":%q,"date":1720000000,"title":"Fixture Patch","feedname":"steam_community_announcements","contents":"UI_BODY_SENTINEL"}]}}`, app, app)
	})
	h := startTestHost(t, f, fixtureID, "GB", "")
	nav := wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, UIVersion: 12, Locale: "zh-Hans"}
	first := viewAnswer(t, h, nav)
	viewAnswer(t, h, wire.UIParams{Contribution: summaryID, Size: wire.SizeCompact, Placement: wire.PlacementOverviewCard, UIVersion: 12})
	if f.hit(newsForAppPath) != 0 {
		t.Fatal("the initial Steam page or card blocked on a news request")
	}
	entry := slices.IndexFunc(first.Sections[0].Items, func(item wire.Component) bool {
		return item.Type == wire.ComponentButton && item.Title == "近期官方更新" && item.Action != nil
	})
	if entry < 0 {
		t.Fatal("the overview is missing the localized news entry")
	}
	withQuery := act(t, h, nav, first, "search", map[string]string{"q": "rally"})
	news := act(t, h, nav, withQuery, first.Sections[0].Items[entry].Action.ID, nil)
	if news.Title != "近期官方更新" || len(news.Sections) != 1 || len(news.Sections[0].Items) != 3 {
		t.Fatalf("unexpected official updates view: %+v", news)
	}
	link := news.Sections[0].Items[1]
	wantURL := storeBase + "/news/app/" + appIDText(firstApp) + "/view/" + appIDText(firstApp)
	if link.Type != wire.ComponentLink || link.URL != wantURL {
		t.Fatalf("expected an official clickable Steam link, got %+v", link)
	}
	raw, _ := json.Marshal(news)
	contains(t, string(raw), "Fixture Patch", "Synthetic Adventure", "2024-07-03")
	assertHidden(t, string(raw), "UI_BODY_SENTINEL")

	answer := toolAnswer(t, h, gameNewsName, `{}`)
	contains(t, answer, wantURL, `"summary_enabled":false`)
	if f.hit(newsForAppPath) != 2 || f.hit(recentPath) != 1 {
		t.Fatal("the page and tool did not reuse the official news and recent games caches")
	}
	if back := act(t, h, nav, news, "back", nil); back.Search == nil || back.Search.Value != "rally" {
		t.Fatal("returning from news lost the search state")
	}
	nav.Locale = "en"
	raw, _ = json.Marshal(viewAnswer(t, h, nav))
	contains(t, string(raw), "Recent official updates")
}

func TestOfficialUpdatesPageDeduplicatesWarningsInOrder(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	f.reply(recentPath, http.StatusOK, `{"response":{"total_count":0}}`)
	f.handle(newsForAppPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("appid") == appIDText(secondApp) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h := startTestHost(t, f, fixtureID, "GB", "")
	nav := wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, UIVersion: 12, Locale: "en"}
	view := act(t, h, nav, viewAnswer(t, h, nav), gameNewsAction, nil)
	want := english.message(problemNetwork, false) + " " + english.message(problemRateLimited, false)
	if got := view.Sections[0].Footer; got != want || f.hit(newsForAppPath) != 3 {
		t.Fatalf("warning footer = %q, want %q from three requests", got, want)
	}
}
