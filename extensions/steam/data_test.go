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
		wantKey := false
		wantHost := apiHost
		switch req.URL.Path {
		case vanityPath, playerPath, ownedPath, recentPath, achievementsPath:
			wantKey = true
		case wishlistPath, storeNamesPath:
		case storePricesPath:
			wantHost = storeHost
		default:
			t.Error("attempted an unexpected outbound request or followed a redirect")
			return nil, fmt.Errorf("unexpected outbound request")
		}
		if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Host != wantHost {
			t.Error("wrong method or origin for a Steam read")
			return nil, fmt.Errorf("unexpected origin")
		}
		secretHeader := req.Header.Get("X-WebAPI-Key")
		if (wantKey && secretHeader != f.key) || (!wantKey && secretHeader != "") ||
			req.URL.Query().Has("key") || strings.Contains(req.URL.String(), f.key) {
			t.Error("credential sent outside its dedicated API header")
			return nil, fmt.Errorf("unexpected credential placement")
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
	progress := d.achievements(ctx, p, firstApp).Data
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
