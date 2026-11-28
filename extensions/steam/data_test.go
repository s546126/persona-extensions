package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	fixtureID            = "76561202255233023"
	fixtureID2           = "76561202255233022"
	fixtureVanity        = "synthetic_arcade_player"
	firstApp      uint32 = 4_000_000_101
	secondApp     uint32 = 4_000_000_102
	thirdApp      uint32 = 4_000_000_103
)

type fakeSteam struct {
	t     *testing.T
	key   string
	http  *http.Client
	mu    sync.Mutex
	paths map[string]http.HandlerFunc
	hits  map[string]int
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newFakeSteam(t *testing.T, timeout time.Duration) *fakeSteam {
	return newFakeSteamMode(t, timeout, "")
}

func newFakeSteamMode(t *testing.T, timeout time.Duration, access string) *fakeSteam {
	t.Helper()
	f := &fakeSteam{t: t, key: rand.Text(), paths: map[string]http.HandlerFunc{}, hits: map[string]int{}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		handler := f.paths[r.URL.Path]
		f.hits[r.URL.Path]++
		f.mu.Unlock()
		if handler == nil {
			t.Error("unexpected request at the synthetic Steam server")
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	fakeURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		wantKey, wantToken := false, false
		wantHost := apiHost
		method := http.MethodGet
		switch req.URL.Path {
		case vanityPath, playerPath, achievementsPath, keyAchievementSchema:
			if access != "" {
				t.Error("QR mode contacted a legacy endpoint")
				return nil, fmt.Errorf("unexpected legacy endpoint")
			}
			wantKey = true
		case ownedPath, recentPath:
			wantKey, wantToken = access == "", access != ""
		case playerLinkPath, wishlistPath:
			wantToken = access != ""
		case progressPath:
			method, wantToken = http.MethodPost, true
		case userAchievementsPath:
			wantToken = true
		case globalAchievementsPath, gameAchievementsPath, newsForAppPath:
		case authBasePath + beginQRPath + "/v1/", authBasePath + pollQRPath + "/v1/":
			method = http.MethodPost
		case storeNamesPath, reviewsAPIPath:
		case storePricesPath, storeSearchPath:
			wantHost = storeHost
		default:
			if !strings.HasPrefix(req.URL.Path, tagCatalogPath) && !strings.HasPrefix(req.URL.Path, appReviewsPath) {
				t.Error("attempted an unexpected outbound request or followed a redirect")
				return nil, fmt.Errorf("unexpected outbound request")
			}
			wantHost = storeHost
		}
		if req.Method != method || req.URL.Scheme != "https" || req.URL.Host != wantHost {
			t.Error("wrong method or origin for a Steam read")
			return nil, fmt.Errorf("unexpected origin")
		}
		secretHeader := req.Header.Get("X-WebAPI-Key")
		q := req.URL.Query()
		if (wantKey && secretHeader != f.key) || (!wantKey && secretHeader != "") ||
			(wantToken && (access == "" || q.Get("access_token") != access)) || (!wantToken && q.Has("access_token")) ||
			q.Has("key") || q.Has("refresh_token") || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" ||
			strings.Contains(req.URL.String(), f.key) || (!wantToken && access != "" && strings.Contains(req.URL.String(), access)) {
			t.Error("credential sent outside its dedicated header or authenticated API route")
			return nil, fmt.Errorf("unexpected credential placement")
		}
		if wantToken && (req.URL.Path == ownedPath || req.URL.Path == recentPath || req.URL.Path == wishlistPath) && q.Get("steamid") != fixtureID {
			t.Error("QR read did not target the signed-in SteamID")
		}
		r := req.Clone(req.Context())
		r.URL.Scheme, r.URL.Host = fakeURL.Scheme, fakeURL.Host
		return server.Client().Transport.RoundTrip(r)
	})
	f.http = steamHTTP(transport, timeout)
	f.seed()
	return f
}

func (f *fakeSteam) handle(path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths[path] = h
}

func (f *fakeSteam) reply(path string, status int, body string) {
	f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func (f *fakeSteam) hit(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeSteam) data(dir, id, cc string, now func() time.Time) *steamData {
	return newSteamData(config{SteamID: id, CountryCode: cc}, f.key, dir, f.http, now)
}

func (f *fakeSteam) seed() {
	f.handle(vanityPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("vanityurl") != fixtureVanity {
			f.t.Error("vanity name was not sent as an encoded query parameter")
		}
		_, _ = fmt.Fprintf(w, `{"response":{"success":1,"steamid":%q}}`, fixtureID)
	})
	f.handle(playerPath, func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("steamids")
		if id != fixtureID && id != fixtureID2 {
			f.t.Error("player summaries did not receive the requested synthetic SteamID")
		}
		_, _ = fmt.Fprintf(w, `{"response":{"players":[{"steamid":%q,"personaname":"Fixture Pilot","personastate":1,"communityvisibilitystate":3,"gameextrainfo":"Synthetic Adventure","gameid":%q}]}}`, id, appIDText(firstApp))
	})
	f.handle(ownedPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("include_appinfo") != "1" || q.Get("include_played_free_games") != "1" || q.Get("steamid") == "" {
			f.t.Error("owned games request is missing account or app details")
		}
		_, _ = fmt.Fprintf(w, `{"response":{"game_count":3,"games":[{"appid":%d,"name":"Zeta Rally","playtime_forever":600},{"appid":%d,"name":"Synthetic Adventure","playtime_forever":200},{"appid":%d,"name":"Alpha Café","playtime_forever":800}]}}`, secondApp, firstApp, thirdApp)
	})
	f.reply(recentPath, 200, fmt.Sprintf(`{"response":{"total_count":2,"games":[{"appid":%d,"name":"Zeta Rally","playtime_2weeks":40,"playtime_forever":600},{"appid":%d,"name":"Synthetic Adventure","playtime_2weeks":90,"playtime_forever":200}]}}`, secondApp, firstApp))
	f.reply(achievementsPath, 200, `{"playerstats":{"success":true,"gameName":"Synthetic Adventure","achievements":[{"achieved":1},{"achieved":0},{"achieved":1}]}}`)
	f.reply(wishlistPath, 200, fmt.Sprintf(`{"response":{"items":[{"appid":%d,"priority":2},{"appid":%d,"priority":1},{"appid":%d,"priority":3}]}}`, secondApp, firstApp, thirdApp))
	f.reply(storeNamesPath, 200, fmt.Sprintf(`{"response":{"store_items":[{"appid":%d,"name":"Synthetic Adventure","success":1},{"appid":%d,"name":"Zeta Rally","success":1},{"appid":%d,"name":"Alpha Café","success":1}]}}`, firstApp, secondApp, thirdApp))
	f.handle(storePricesPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("filters") != "price_overview" || q.Get("appids") != fmt.Sprintf("%d,%d,%d", firstApp, secondApp, thirdApp) {
			f.t.Error("Store did not receive one batched price lookup")
		}
		currency, salePrice, fullPrice := "USD", "$3.99", "$6.99"
		if cc := q.Get("cc"); cc == "GB" {
			currency, salePrice, fullPrice = "GBP", "£3.99", "£6.99"
		} else if cc != "US" {
			f.t.Error("Store did not receive a normalized country code")
		}
		_, _ = fmt.Fprintf(w, `{"%d":{"success":true,"data":{"price_overview":{"currency":%q,"final":399,"final_formatted":%q,"discount_percent":40}}},"%d":{"success":true,"data":{"price_overview":{"currency":%q,"final":699,"final_formatted":%q,"discount_percent":0}}},"%d":{"success":true,"data":[]}}`, firstApp, currency, salePrice, secondApp, currency, fullPrice, thirdApp)
	})
}

func TestQRReadRoutes(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	access := fixtureJWT(t, fixtureID, now.Add(time.Hour), "read-access")
	saved := fixtureSecret(t, fixtureID, now)
	f := newFakeSteamMode(t, time.Second, access)
	f.handle(authBasePath+beginQRPath+"/v1/", func(w http.ResponseWriter, _ *http.Request) {
		authFixture(w, "1", beginFixture())
	})
	f.handle(authBasePath+pollQRPath+"/v1/", func(w http.ResponseWriter, _ *http.Request) {
		authFixture(w, "1", grantFixture(saved.RefreshToken, access))
	})
	f.handle(playerLinkPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("steamids[0]") != fixtureID {
			t.Error("GET GetPlayerLinkDetails did not send the SteamID array")
		}
		_, _ = fmt.Fprintf(w, `{"response":{"accounts":[{"public_data":{"steamid":%q,"persona_name":"Other Account"}},{"public_data":{"steamid":%q,"persona_name":"Private QR Pilot","visibility_state":1},"private_data":{"persona_state":1,"game_id":%q,"game_extra_info":"Synthetic Adventure"}}]}}`, fixtureID2, fixtureID, appIDText(firstApp))
	})
	f.handle(progressPath, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SteamID  string   `json:"steamid"`
			Language string   `json:"language"`
			AppIDs   []uint32 `json:"appids"`
		}
		err := r.ParseForm()
		if err != nil || r.URL.Query().Has("input_json") ||
			json.Unmarshal([]byte(r.PostForm.Get("input_json")), &in) != nil || in.SteamID != fixtureID ||
			(in.Language != "schinese" && in.Language != "english") || len(in.AppIDs) != 1 || in.AppIDs[0] != firstApp {
			t.Error("achievement progress did not receive localized input_json in the POST form")
		}
		_, _ = fmt.Fprintf(w, `{"response":{"achievement_progress":[{"appid":%d,"unlocked":1,"total":1},{"appid":%d,"unlocked":2,"total":3}]}}`, secondApp, firstApp)
	})
	f.handle(storeNamesPath, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Context struct {
				Country  string `json:"country_code"`
				Language string `json:"language"`
			} `json:"context"`
		}
		if json.Unmarshal([]byte(r.URL.Query().Get("input_json")), &in) != nil || in.Context.Country != "GB" {
			t.Error("StoreBrowse did not receive the selected country")
		}
		name := "Synthetic Adventure"
		switch in.Context.Language {
		case "schinese":
			name = "示例冒险"
		case "english":
		default:
			t.Error("StoreBrowse did not receive a supported locale")
		}
		_, _ = fmt.Fprintf(w, `{"response":{"store_items":[{"appid":%d,"name":%q,"success":1}]}}`, firstApp, name)
	})

	dir := t.TempDir()
	t.Setenv("PERSONA_EXTENSION_DATA_DIR", dir)
	legacy := newFakeSteam(t, time.Second)
	old := legacy.data(dir, fixtureID, "GB", time.Now)
	if got := old.account(t.Context(), presenceTTL, false); got.Data == nil {
		t.Fatal("could not seed a v0.1 cache for the same SteamID")
	}
	file := filepath.Join(dir, "steam-cache-v1", "profile.json")
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{}
	h := exttest.StartWith(t, manifest, exttest.Serve(t, a.program(f.http, func() time.Time { return now })), func(p *wire.InitializeParams) {
		p.Config = json.RawMessage(fmt.Sprintf(`{"auth_mode":"qr","steam_id":%q,"country_code":"GB","inbox_events_enabled":true,"inbox_max_events_per_day":7,"discovery_tools_enabled":false}`, fixtureID2))
		p.Secrets = map[string]string{keyName: f.key}
		p.Hub.Locale = "zh-Hans"
		p.Extension.DataDir = dir
	})
	contains(t, toolAnswer(t, h, "steam_profile", "{}"), `"available":false`, string(problemAuthRequired))
	if f.hit(playerLinkPath) != 0 {
		t.Fatal("a cold QR install tried to read Steam")
	}
	authStep(t, h, "")
	await(t, 5*time.Second, func() bool { return authStep(t, h, qrStep).Done })
	await(t, time.Second, func() bool {
		return f.hit(wishlistPath) == 1 && f.hit(storeNamesPath) == 1 && f.hit(storePricesPath) == 1
	})
	profile := toolAnswer(t, h, "steam_profile", "{}")
	contains(t, profile, "Private QR Pilot", `"public":false`, `"state":"online"`, "Synthetic Adventure", `"owned_game_count":3`)
	assertHidden(t, profile, access, saved.RefreshToken, f.key, "Other Account")
	contains(t, toolAnswer(t, h, "steam_recent_games", "{}"), `"available":true`, `"playtime_2weeks":130`)
	contains(t, toolAnswer(t, h, "steam_library", `{"query":"zeta"}`), "Zeta Rally", `"playtime_forever":600`)
	contains(t, toolAnswer(t, h, "steam_achievements", fmt.Sprintf(`{"appid":%d}`, firstApp)), `"total":3`, `"unlocked":2`)
	contains(t, toolAnswer(t, h, "steam_wishlist", `{"limit":1}`), "示例冒险", `"country_code":"GB"`, `"currency":"GBP"`, `"discounted_count":1`)
	for path, want := range map[string]int{playerLinkPath: 1, ownedPath: 1, recentPath: 1, progressPath: 1, wishlistPath: 2, storeNamesPath: 2, storePricesPath: 2} {
		if n := f.hit(path); n != want {
			t.Errorf("expected %d %s reads, got %d", want, path, n)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	persisted, readErr := os.ReadFile(file)
	if err != nil || readErr != nil || len(entries) != 1 || string(persisted) != string(original) {
		t.Fatal("QR reads wrote into the existing Key-mode cache")
	}
	if _, err := os.Lstat(filepath.Join(dir, storeDirectory)); !os.IsNotExist(err) {
		t.Fatalf("QR reads created a disk snapshot store: %v", err)
	}
	d, _, cancel := a.readContext(t.Context())
	cancel()
	if d.config.InboxEventLimit() != 7 || d.config.DiscoveryResultLimit() != 0 {
		t.Fatal("QR session lost the configured token controls")
	}
	playtime, err := d.stores.playtime.History(fixtureID)
	if err != nil || len(playtime) != 1 || playtime[0].TotalMinutes != 1600 {
		t.Fatalf("QR playtime was not recorded in memory: %+v, %v", playtime, err)
	}
	observed, err := d.stores.prices.Histories(fixtureID, "GB", firstApp)
	if err != nil || observed[firstApp].Latest.Final != 399 {
		t.Fatalf("QR prices were not recorded in memory: %+v, %v", observed, err)
	}

	nav := wire.UIParams{Contribution: extensionID, Size: wire.SizeFull, Placement: wire.PlacementNavPage, UIVersion: 12, Locale: "en"}
	view := viewAnswer(t, h, nav)
	detail := act(t, h, nav, view, fmt.Sprintf("library-game-%d", firstApp), nil)
	if len(detail.Sections) != 3 || detail.Sections[1].Items[0].Detail != "2 of 3 unlocked" {
		t.Fatal("the QR game detail did not draw achievement progress")
	}
	act(t, h, nav, detail, "backlog-save", map[string]string{"backlog_status": "queued"})
	if got, ok, err := NewBacklogStore(dir).Get(fixtureID, firstApp); err != nil || !ok || got.Status != BacklogQueued {
		t.Fatalf("the QR page did not persist the local status: %+v, %t, %v", got, ok, err)
	}
	f.reply(playerLinkPath, 200, fmt.Sprintf(`{"response":{"accounts":[{"public_data":{"steamid":%q,"persona_name":"Private QR Pilot","visibility_state":1}}]}}`, fixtureID))
	f.handle(recentPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-EResult", "15")
		_, _ = io.WriteString(w, `{"response":{"total_count":0}}`)
	})
	f.reply(progressPath, 200, `{"response":{}}`)
	view = act(t, h, nav, view, "refresh", nil)
	if len(view.Sections) != 5 || view.Sections[0].Items[0].Subtitle != english.unknown ||
		view.Sections[1].Items[0].Title != english.message(problemGameDetails, false) ||
		view.Sections[2].Items[0].Title != "Alpha Café" || view.Sections[3].Items[0].Title != "Synthetic Adventure" {
		t.Fatal("missing presence or a denied dataset hid the remaining QR reads")
	}
	contains(t, toolAnswer(t, h, "steam_achievements", fmt.Sprintf(`{"appid":%d}`, firstApp)), `"available":false`, string(problemAchievements))
	f.reply(playerLinkPath, 200, `{"response":{"accounts":[]}}`)
	f.reply(wishlistPath, http.StatusForbidden, "hidden")
	prices := f.hit(storePricesPath)
	if got := act(t, h, nav, view, "refresh", nil); len(got.Sections) != 5 || got.Sections[2].Items[0].Title != "Alpha Café" ||
		got.Sections[3].Items[0].Title != english.message(problemWishlist, false) || f.hit(storePricesPath) != prices {
		t.Fatal("library reads should use the signed-in SteamID even if the profile is unavailable")
	}
	var status wire.AuthStatus
	if err := h.Call("auth.logout", struct{}{}, &status); err != nil || status.State != wire.AuthStateSignedOut {
		t.Fatalf("QR logout failed: %v, %q", err, status.State)
	}
	playtime, err = d.stores.playtime.History(fixtureID)
	observed, priceErr := d.stores.prices.Histories(fixtureID, "GB", firstApp)
	if err != nil || priceErr != nil || len(playtime) != 0 || len(observed) != 0 {
		t.Fatal("QR logout retained private playtime or price observations")
	}
}

func TestQRReadErrorRedactsURLError(t *testing.T) {
	t.Parallel()
	now := time.Now()
	saved := fixtureSecret(t, fixtureID, now)
	access := fixtureJWT(t, fixtureID, now.Add(time.Hour), "error-access")
	f := newAuthServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		authFixture(w, "1", refreshFixture(access, ""))
	})
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, authBasePath) {
			return f.Transport.RoundTrip(r)
		}
		if r.URL.Query().Get("access_token") != access {
			t.Error("test did not exercise a credential-bearing URL")
		}
		return nil, &url.Error{Op: r.Method, URL: r.URL.String(),
			Err: fmt.Errorf("%s %s %s: %w", saved.RefreshToken, requestID, firstQR, context.DeadlineExceeded)}
	})
	h, root, hub := testSecretHub(t)
	q, _ := restoredSession(t, h, root, hub, steamHTTP(transport, time.Second), func() time.Time { return now }, saved)
	a := &app{steamData: &steamData{config: config{AuthMode: modeQR}}, qr: q, locale: "en"}
	out, err := a.callTool(t.Context(), "steam_profile", nil)
	if err != nil {
		t.Fatal("a network error escaped the tool")
	}
	contains(t, out.Text, `"available":false`, `"code":"timeout"`)
	v := a.page(t.Context(), &english, pageState{Sort: sortPlaytime}, false)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	assertHidden(t, out.Text+string(raw), access, saved.RefreshToken, requestID, firstQR, "access_token=", "steamids", apiBase)
}

func TestSteamAccessTokenCannotFollowRedirect(t *testing.T) {
	count := 0
	c := steamClient{http: steamHTTP(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count++
		if count != 1 || r.URL.Host != apiHost || r.URL.Query().Get("access_token") != "synthetic-access" {
			t.Error("credential left its authorised host")
		}
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{
			"Location": {storeBase + "/api/appdetails?access_token=synthetic-access"},
		}, Body: http.NoBody}, nil
	}), time.Second), access: "synthetic-access"}
	if _, issue := c.profile(t.Context(), fixtureID); issue != problemResponse || count != 1 {
		t.Fatal("credential-bearing request should stop at a redirect")
	}
	if issue := c.get(t.Context(), storeBase+storePricesPath, nil, true, &struct{}{}); issue != problemResponse || count != 1 {
		t.Fatal("credential-bearing request should not be sent to the Store")
	}
}

func TestLogoutRejectsLateReadsAndClearsSlots(t *testing.T) {
	t.Parallel()
	now := time.Now()
	saved := fixtureSecret(t, fixtureID, now)
	oldToken := fixtureJWT(t, fixtureID, now.Add(time.Hour), "old-access")
	newToken := fixtureJWT(t, fixtureID, now.Add(time.Hour), "new-access")
	f := newAuthServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		authFixture(w, "1", refreshFixture(oldToken, ""))
	})
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case authBasePath + renewPath + "/v1/":
			return f.Transport.RoundTrip(r)
		case playerLinkPath:
			body = fmt.Sprintf(`{"response":{"accounts":[{"public_data":{"steamid":%q,"persona_name":"Old Pilot"}}]}}`, fixtureID)
		case ownedPath:
			if r.URL.Query().Get("access_token") == oldToken {
				entered <- r.Context()
				<-release // Simulate an upstream response that arrives despite cancellation.
				body = fmt.Sprintf(`{"response":{"game_count":1,"games":[{"appid":%d,"name":"Late old library"}]}}`, firstApp)
			} else {
				body = fmt.Sprintf(`{"response":{"game_count":1,"games":[{"appid":%d,"name":"New generation library"}]}}`, secondApp)
			}
		default:
			t.Error("unexpected read in the logout race")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	h, root, hub := testSecretHub(t)
	q, s := restoredSession(t, h, root, hub, steamHTTP(transport, time.Second), func() time.Time { return now }, saved)
	a := &app{steamData: &steamData{config: config{AuthMode: modeQR}}, qr: q, locale: "en"}
	if p := s.reads.account(t.Context(), presenceTTL, false); p.Data == nil {
		t.Fatal("could not warm the QR profile cache")
	}
	out := make(chan wire.ConnectorCallResult, 1)
	go func() {
		r, err := a.callTool(t.Context(), "steam_library", nil)
		if err != nil {
			t.Error("old generation failed at the tool boundary")
		}
		out <- r
	}()

	var inFlight context.Context
	select {
	case inFlight = <-entered:
	case <-time.After(time.Second):
		t.Fatal("the old read did not start")
	}
	if st, err := a.Logout(t.Context()); err != nil || st.State != wire.AuthStateSignedOut {
		t.Fatal("logout did not close the old session")
	}
	view := a.page(t.Context(), &english, pageState{AppID: firstApp, From: librarySection}, false)
	if len(view.State) != 0 || view.Title != "Steam" {
		t.Fatal("logout did not discard the prior game detail state")
	}
	select {
	case <-inFlight.Done():
	case <-time.After(time.Second):
		t.Fatal("logout did not cancel the in-flight read")
	}
	// The same account may sign in again while the old HTTP response is still blocked.
	q.mu.Lock()
	q.installLocked(steamTokens{secret: saved, refreshExpiry: now.Add(time.Hour), access: newToken, accessExpiry: now.Add(time.Hour)})
	q.mu.Unlock()
	got, err := a.callTool(t.Context(), "steam_library", nil)
	if err != nil {
		t.Fatal(err)
	}
	contains(t, got.Text, "New generation library")
	unblock()
	select {
	case result := <-out:
		contains(t, result.Text, `"available":false`)
		assertHidden(t, result.Text, "Late old library", "New generation library")
	case <-time.After(time.Second):
		t.Fatal("the canceled read did not return")
	}
	if history, err := s.reads.stores.playtime.History(fixtureID); err != nil || len(history) != 0 {
		t.Fatal("a late read refilled the signed-out session's playtime snapshots")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if s.reads.profile.record.Data != nil || s.reads.library.record.Data != nil ||
		q.session.reads.library.record.Data == nil || q.session.generation == s.generation {
		t.Fatal("logout failed to clear the old cache or let it be refilled")
	}
}

func TestSteamReads(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	d := f.data(t.TempDir(), fixtureVanity, " gb ", time.Now)
	ctx := t.Context()
	p := d.account(ctx, presenceTTL, false)
	if p.Data == nil || p.Data.SteamID != fixtureID || p.Data.Name != "Fixture Pilot" ||
		p.Data.PlayApp != firstApp || p.Data.State != "online" || p.Data.Playing != "Synthetic Adventure" || !p.Data.Public {
		t.Fatalf("player %+v", p.Data)
	}
	if matches := libraryGames(d.ownedGames(ctx, p, false).Data.Games, "CAFÉ", sortName); len(matches) != 1 || matches[0].AppID != thirdApp {
		t.Fatalf("library search %+v", matches)
	}
	got := d.recentGames(ctx, p, false).Data
	if got.Playtime2Weeks != 130 || len(got.Games) != 2 || got.Games[0].AppID != firstApp || got.Games[0].PlaytimeForever != 200 {
		t.Fatalf("recent games %+v", got)
	}
	progress := d.achievements(ctx, p, firstApp, "english").Data
	if progress == nil || progress.Unlocked != 2 || progress.Total != 3 {
		t.Fatalf("achievements %+v", progress)
	}
	w := d.wishlist(ctx, p, "english", false)
	if w.Data == nil || w.Data.Discounted != 1 || w.Data.Checked != 3 || w.Data.Total != 3 {
		t.Fatalf("wishlist %+v", w)
	}
	item := w.Data.Items[0]
	if item.Name != "Synthetic Adventure" || item.Price == nil || item.Price.Currency != "GBP" || item.Price.Final != 399 ||
		item.Price.DiscountPercent != 40 || w.Data.Items[2].Price != nil || !w.Data.Items[2].PriceChecked {
		t.Fatalf("wishlist item %+v", item)
	}
}

func TestRefreshFailureStaysVisible(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	d := f.data(t.TempDir(), fixtureID, "", time.Now)
	p := d.account(t.Context(), profileTTL, false)
	if r := d.ownedGames(t.Context(), p, false); r.Data == nil || r.Stale {
		t.Fatalf("initial library %+v", r)
	}
	f.reply(ownedPath, http.StatusTooManyRequests, "rate limited")
	for _, refresh := range []bool{true, false} {
		r := d.ownedGames(t.Context(), p, refresh)
		if r.Data == nil || r.Data.Total != 3 || !r.Stale || r.Issue != problemRateLimited {
			t.Fatalf("refresh=%t: expected a visible stale notice, got %+v", refresh, r)
		}
	}
	if c := f.hit(ownedPath); c != 2 {
		t.Fatalf("library read %d times", c)
	}
}

func TestSteamCacheLayersAndScope(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	now := time.Now()
	clock := func() time.Time { return now }
	d := f.data(dir, fixtureVanity, "", clock)
	load := func() {
		t.Helper()
		p := d.account(t.Context(), presenceTTL, false)
		if p.Data == nil || d.ownedGames(t.Context(), p, false).Data == nil ||
			d.recentGames(t.Context(), p, false).Data == nil || d.wishlist(t.Context(), p, "english", false).Data == nil {
			t.Fatal("missing cached or upstream data")
		}
	}
	count := func(path string, want int) {
		t.Helper()
		if got := f.hit(path); got != want {
			t.Errorf("%s: %d requests, want %d", path, got, want)
		}
	}
	load()
	d = f.data(dir, fixtureVanity, "US", clock)
	load()
	for _, path := range []string{vanityPath, playerPath, ownedPath, recentPath, wishlistPath, storePricesPath} {
		count(path, 1)
	}
	now = now.Add(11 * time.Minute)
	load()
	count(vanityPath, 1)
	count(playerPath, 2)
	count(ownedPath, 1)
	count(recentPath, 2)
	count(storePricesPath, 2)

	d = f.data(dir, fixtureVanity, "GB", clock)
	load()
	count(vanityPath, 1)
	count(playerPath, 2)
	count(ownedPath, 1)
	count(storePricesPath, 3)

	now = now.Add(8 * time.Hour)
	load()
	count(vanityPath, 2)
	count(ownedPath, 2)

	d = f.data(dir, fixtureID2, "GB", clock)
	load()
	count(ownedPath, 3)
	count(storePricesPath, 5)
	files, err := os.ReadDir(filepath.Join(dir, "steam-cache-v1"))
	if err != nil || len(files) != 5 {
		t.Fatalf("expected five fixed cache slots, got %d: %v", len(files), err)
	}
	for _, file := range files {
		info, err := file.Info()
		if err != nil || info.Mode().Perm() != 0o600 || info.Size() > cacheMaxSize {
			t.Fatalf("unexpected cache permissions or size for %s", file.Name())
		}
	}
}

func TestPrivateDataClearsCachedLibrary(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	ctx := t.Context()
	d := f.data(dir, fixtureID, "", time.Now)
	p := d.account(ctx, profileTTL, false)
	if r := d.ownedGames(ctx, p, false); r.Data == nil {
		t.Fatal("warm library unavailable")
	}
	f.reply(ownedPath, 200, `{"response":{}}`)
	if r := d.ownedGames(ctx, p, true); r.Data != nil || r.Issue != problemGameDetails {
		t.Fatalf("private library %+v", r)
	}
	f.reply(ownedPath, 429, "limited")
	d = f.data(dir, fixtureID, "", time.Now)
	if r := d.ownedGames(ctx, p, false); r.Data != nil || r.Issue != problemRateLimited {
		t.Fatalf("private cache revived after process restart: %+v", r)
	}
}

func TestConcurrentColdCacheReads(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	d := f.data(t.TempDir(), fixtureID, "", time.Now)
	p := d.account(t.Context(), profileTTL, false)
	var calls sync.WaitGroup
	for range 12 {
		calls.Go(func() {
			if got := d.ownedGames(t.Context(), p, false); got.Data == nil || got.Data.Total != 3 {
				t.Errorf("library cache returned %+v", got)
			}
		})
	}
	calls.Wait()
	if got := f.hit(ownedPath); got != 1 {
		t.Fatalf("concurrent readers made %d library requests", got)
	}
}

func TestStorePriceBatchesAreBounded(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	items := make([]wishItem, maxWishlistItems+20)
	for i := range items {
		items[i] = wishItem{AppID: firstApp + uint32(i), Priority: uint32(i + 1)}
	}
	payload, err := json.Marshal(map[string]any{"response": map[string]any{"items": items}})
	if err != nil {
		t.Fatal(err)
	}
	f.reply(wishlistPath, 200, string(payload))
	f.handle(storePricesPath, func(w http.ResponseWriter, r *http.Request) {
		apps := strings.Split(r.URL.Query().Get("appids"), ",")
		if len(apps) != storeBatchSize {
			t.Errorf("batch size %d", len(apps))
		}
		result := map[string]any{}
		for _, app := range apps {
			id, _ := strconv.ParseUint(app, 10, 32)
			if id >= uint64(firstApp)+maxWishlistItems {
				t.Error("looked up prices beyond the snapshot limit")
			}
			result[app] = map[string]any{"success": false}
		}
		_ = json.NewEncoder(w).Encode(result)
	})
	got, issue := (steamClient{http: f.http, key: f.key}).wishlist(t.Context(), fixtureID, defaultCountry, "english")
	if issue != "" || got.Total != len(items) || got.Checked != maxWishlistItems || len(got.Items) != maxWishlistItems {
		t.Fatalf("bounded snapshot issue=%q, items=%d, checked=%d, total=%d", issue, len(got.Items), got.Checked, got.Total)
	}
	if requests := f.hit(storePricesPath); requests != maxWishlistItems/storeBatchSize {
		t.Fatalf("%d Store price requests", requests)
	}
}

func TestSteamRedirectAndEchoCannotExposeKey(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	d := f.data(t.TempDir(), fixtureID, "", time.Now)
	ctx := t.Context()
	f.handle(playerPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storeBase+storePricesPath, http.StatusFound)
	})
	if p := d.account(ctx, profileTTL, true); p.Data != nil || p.Issue != problemResponse || f.hit(storePricesPath) != 0 {
		t.Fatal("unexpected success or redirect from the key-bearing endpoint")
	}
	f.reply(playerPath, 200, fmt.Sprintf(`{"response":{"players":[{"steamid":%q,"personaname":%q,"communityvisibilitystate":3,"gameextrainfo":%q}]}}`, fixtureID, f.key, f.key))
	p := d.account(ctx, profileTTL, true)
	if p.Data == nil || p.Data.Name != "[redacted]" || p.Data.Playing != "[redacted]" {
		t.Fatal("API response could expose the credential")
	}
	raw, err := os.ReadFile(d.profile.path)
	if err != nil || strings.Contains(string(raw), f.key) {
		t.Fatal("unsafe cache record")
	}
	f.reply(playerPath, http.StatusForbidden, f.key)
	if got := d.account(ctx, profileTTL, true); got.Data != nil || got.Issue != problemKey {
		t.Fatal("a credential error returned unsanitized or cached data")
	}
}

func TestHTTPTimeoutAndExpiredSnapshot(t *testing.T) {
	f := newFakeSteam(t, 50*time.Millisecond)
	now := time.Now()
	d := f.data(t.TempDir(), fixtureID, "", func() time.Time { return now })
	ctx := context.Background()
	p := d.account(ctx, profileTTL, false)
	if r := d.recentGames(ctx, p, false); r.Data == nil {
		t.Fatal("initial games unavailable")
	}
	f.handle(recentPath, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	now = now.Add(recentTTL)
	if r := d.recentGames(ctx, p, false); r.Data == nil || !r.Stale || r.Issue != problemTimeout {
		t.Fatalf("timeout did not reuse a labeled snapshot: %+v", r)
	}
	now = now.Add(maxStaleAge)
	if r := d.recentGames(ctx, p, false); r.Data != nil || r.Issue != problemTimeout {
		t.Fatalf("old snapshot survived its age limit: %+v", r)
	}
}
