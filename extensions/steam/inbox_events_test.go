package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func startInboxTestHost(t *testing.T, f *fakeSteam, a *app, now func() time.Time, dir, locale, settings string) *exttest.Host {
	t.Helper()
	t.Setenv("PERSONA_EXTENSION_DATA_DIR", dir)
	return exttest.StartWith(t, manifest, exttest.Serve(t, a.program(f.http, now)), func(p *wire.InitializeParams) {
		p.Config = json.RawMessage(fmt.Sprintf(`{"steam_id":%q,%s}`, fixtureID, settings))
		p.Secrets = map[string]string{keyName: f.key}
		p.Hub.Locale = locale
	})
}

func collectWishlistInbox(t *testing.T, h *exttest.Host) []wire.ConnectorInboxThing {
	t.Helper()
	var out wire.ConnectorCollectResult
	if err := h.Call("connector.inbox.collect", wire.ConnectorParams{Contribution: extensionID}, &out); err != nil {
		t.Fatal(err)
	}
	return out.Things
}

func TestWishlistInboxDisabled(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	a := &app{}
	h := startInboxTestHost(t, f, a, time.Now, dir, "en", `"sale_roundup_enabled":true,"wishlist_discount_threshold":1`)
	if got := collectWishlistInbox(t, h); len(got) != 0 {
		t.Fatalf("default-off inbox returned %d things", len(got))
	}
	for _, path := range []string{playerPath, wishlistPath, storeNamesPath, storePricesPath} {
		if f.hit(path) != 0 {
			t.Fatalf("default-off Inbox requested %s", path)
		}
	}
	contains(t, toolAnswer(t, h, "steam_wishlist", `{"limit":1}`), "Synthetic Adventure")
	a.scanWishlistInbox(t.Context(), &english, func() { t.Error("default-off inbox notified") })
	if got := collectWishlistInbox(t, h); len(got) != 0 || hasEvent(h, "connector.changed") || f.hit(wishlistPath) != 1 {
		t.Fatal("the static wishlist read produced an Inbox event")
	}
	if _, err := os.Stat(filepath.Join(dir, storeDirectory, "events.json")); !os.IsNotExist(err) {
		t.Fatalf("the default-off inbox touched the cursor store: %v", err)
	}
}

type inboxFixtureGame struct {
	appid    uint32
	final    int // negative when Steam lists no price
	discount int
	metadata string
}

func seedInboxWishlist(t *testing.T, f *fakeSteam, games ...inboxFixtureGame) {
	t.Helper()
	wishes := make([]wishItem, 0, len(games))
	items := make([]map[string]any, 0, len(games))
	prices := make(map[string]any, len(games))
	for i, game := range games {
		wishes = append(wishes, wishItem{AppID: game.appid, Priority: uint32(i)})
		details := map[string]any{"appid": game.appid, "success": 1, "name": fmt.Sprintf("Fixture Game %d", i+1)}
		if game.metadata != "" {
			requireStoreOK(t, json.Unmarshal([]byte(game.metadata), &details))
		}
		items = append(items, details)
		var price any = []any{}
		if game.final >= 0 {
			price = map[string]any{"price_overview": storePrice{Currency: "USD", Final: uint32(game.final), DiscountPercent: game.discount}}
		}
		prices[appIDText(game.appid)] = map[string]any{"success": true, "data": price}
	}
	reply := func(dataset string, data any) string {
		raw, err := json.Marshal(map[string]any{"response": map[string]any{dataset: data}})
		requireStoreOK(t, err)
		return string(raw)
	}
	f.reply(wishlistPath, http.StatusOK, reply("items", wishes))
	f.reply(storeNamesPath, http.StatusOK, reply("store_items", items))
	raw, err := json.Marshal(prices)
	requireStoreOK(t, err)
	f.reply(storePricesPath, http.StatusOK, string(raw))
}

func newInboxTestApp(f *fakeSteam, dir string, now func() time.Time, cfg config) *app {
	cfg.SteamID = fixtureID
	return &app{steamData: newSteamData(cfg, f.key, dir, f.http, now)}
}

func scanTestInbox(t *testing.T, a *app, locale *text) (things []wire.ConnectorInboxThing, changed bool) {
	t.Helper()
	a.scanWishlistInbox(t.Context(), locale, func() { changed = true })
	return a.collectWishlistInbox().Things, changed
}

func TestWishlistInboxWireCollect(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	seedInboxWishlist(t, f, inboxFixtureGame{firstApp, 500, 50, fmt.Sprintf(`{"name":%q}`, "Fixture "+fixtureID+" "+f.key)})
	var metadataRequested atomic.Bool
	f.handle(storeNamesPath, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			DataRequest map[string]bool `json:"data_request"`
		}
		metadataRequested.Store(json.Unmarshal([]byte(r.URL.Query().Get("input_json")), &in) == nil && in.DataRequest["include_release"] && in.DataRequest["include_best_purchase_option"])
		_, _ = fmt.Fprintf(w, `{"response":{"store_items":[{"appid":%d,"success":1,"name":%q}]}}`, firstApp, "Fixture "+fixtureID+" "+f.key)
	})
	h := startInboxTestHost(t, f, &app{}, time.Now, t.TempDir(), "zh-Hans", `"inbox_events_enabled":true`)
	await(t, 3*time.Second, func() bool { return hasEvent(h, "connector.changed") })
	var event wire.ConnectorChangedEvent
	for _, received := range h.Events() {
		if received.Name == "connector.changed" {
			requireStoreOK(t, json.Unmarshal(received.Params, &event))
		}
	}
	if event.Contribution != extensionID || event.Scope != wire.ConnectorScopeInbox {
		t.Fatalf("unexpected notification %+v", event)
	}
	got := collectWishlistInbox(t, h)
	if len(got) != 1 || got[0].Kind != wishlistPriceKind || got[0].URL != storeLink(firstApp) ||
		got[0].Thread == "" || got[0].Last == "" || got[0].At.IsZero() || got[0].CanReply ||
		got[0].Tier != wire.ConnectorTierAction || got[0].Extra == nil || got[0].Extra.RuleTier != wire.ConnectorTierAction {
		t.Fatalf("unexpected Inbox things %+v", got)
	}
	contains(t, got[0].Title, "愿望单价格提醒")
	contains(t, got[0].Preview, "折扣达到 50%")
	raw, err := json.Marshal(got)
	requireStoreOK(t, err)
	assertHidden(t, string(raw), fixtureID, f.key)
	if again := collectWishlistInbox(t, h); !reflect.DeepEqual(got, again) {
		t.Fatalf("collection changed stable values %+v", again)
	}
	if !metadataRequested.Load() || f.hit(playerPath) != 1 || f.hit(wishlistPath) != 1 || f.hit(storeNamesPath) != 1 || f.hit(storePricesPath) != 1 {
		t.Fatal("collection should use a single pre-scanned snapshot")
	}
}

func TestWishlistInboxPriceTriggers(t *testing.T) {
	for _, tc := range []struct {
		name            string
		final, discount int
		threshold       int
		target, want    string
		baseline        *PriceQuote
	}{
		{name: "first observation below threshold", final: 310, discount: 49},
		{name: "exact default threshold", final: 300, discount: 50, want: "At least 50% off"},
		{name: "custom threshold", final: 230, discount: 70, threshold: 75},
		{name: "target at regular price", final: 330, target: "USD:330", want: "Target price reached"},
		{name: "target currency mismatch", final: 330, target: "GBP:330"},
		{name: "new local low", final: 248, discount: 20, baseline: &PriceQuote{Currency: "USD", Final: 249}, want: "New locally observed low"},
		{name: "equal local low", final: 249, discount: 20, baseline: &PriceQuote{Currency: "USD", Final: 249}},
		{name: "first quote in another currency", final: 199, discount: 20, baseline: &PriceQuote{Currency: "GBP", Final: 249}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSteam(t, time.Second)
			seedInboxWishlist(t, f, inboxFixtureGame{firstApp, tc.final, tc.discount, ""})
			now := fixtureObservationAt
			var targets string
			if tc.target != "" {
				targets = appIDText(firstApp) + ":" + tc.target
			}
			a := newInboxTestApp(f, t.TempDir(), func() time.Time { return now }, config{
				TokenConfig: TokenConfig{InboxEventsEnabled: true}, WishlistDiscountThreshold: tc.threshold,
				WishlistTargetPrices: targets,
			})
			if tc.baseline != nil {
				requireStoreOK(t, a.stores.prices.Observe(fixtureID, "US", now.Add(-time.Hour), map[uint32]PriceQuote{firstApp: *tc.baseline}))
			}
			things, changed := scanTestInbox(t, a, &english)
			if tc.want == "" {
				if len(things) != 0 || changed {
					t.Fatalf("unexpected first price event %+v", things)
				}
				return
			}
			if len(things) != 1 || !changed || things[0].Kind != wishlistPriceKind {
				t.Fatalf("expected one price event, got %+v", things)
			}
			contains(t, things[0].Preview, tc.want)
			now = now.Add(5 * time.Minute)
			if again, changed := scanTestInbox(t, a, &english); changed || !reflect.DeepEqual(things, again) {
				t.Fatalf("re-scanning the same price changed the inbox: %+v", again)
			}
		})
	}
}

func TestWishlistInboxUsesLowSeenByStaticWishlist(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	now := fixtureObservationAt
	a := newInboxTestApp(f, t.TempDir(), func() time.Time { return now }, config{TokenConfig: TokenConfig{InboxEventsEnabled: true}})
	seedInboxWishlist(t, f, inboxFixtureGame{firstApp, 950, 0, ""})
	if got, changed := scanTestInbox(t, a, &english); len(got) != 0 || changed {
		t.Fatalf("seed price created an alert: %+v", got)
	}
	seedInboxWishlist(t, f, inboxFixtureGame{firstApp, 850, 10, ""})
	now = now.Add(time.Minute)
	p := a.account(t.Context(), profileTTL, false)
	if r := a.wishlist(t.Context(), p, "english", true); r.Issue != "" {
		t.Fatal(r.Issue)
	}
	now = now.Add(time.Minute)
	got, changed := scanTestInbox(t, a, &english)
	if len(got) != 1 || !changed {
		t.Fatalf("a foreground price observation swallowed the new low: %+v", got)
	}
	contains(t, got[0].Preview, "New locally observed low")
}

func TestWishlistInboxReleaseDemoAndGiveaways(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	now := fixtureObservationAt
	a := newInboxTestApp(f, t.TempDir(), func() time.Time { return now }, config{TokenConfig: TokenConfig{InboxEventsEnabled: true}})
	seedInboxWishlist(t, f,
		inboxFixtureGame{firstApp, -1, 0, `{"release":{"is_coming_soon":true}}`},
		inboxFixtureGame{firstApp + 8, -1, 0, `{"is_coming_soon":false}`},
	)
	if got, changed := scanTestInbox(t, a, &english); len(got) != 0 || changed {
		t.Fatalf("initial release state created an alert: %+v", got)
	}
	now = now.Add(wishlistPollInterval)
	ends := now.Add(time.Hour)
	seedInboxWishlist(t, f,
		inboxFixtureGame{firstApp, -1, 0, fmt.Sprintf(`{"release":{"steam_release_date":%d}}`, now.Unix())},
		inboxFixtureGame{secondApp, -1, 0, `{"related_items":{"demo_appid":[4000000199],"standalone_demos":[{"appid":4000000199}]}}`},
		inboxFixtureGame{thirdApp, 0, 100, ""},
		inboxFixtureGame{firstApp + 3, -1, 0, fmt.Sprintf(`{"best_purchase_option":{"is_free_to_keep":true,"free_to_keep_ends":%d}}`, ends.Unix())},
		inboxFixtureGame{firstApp + 4, 0, 100, `{"is_free":true}`},
		inboxFixtureGame{firstApp + 5, 0, 100, fmt.Sprintf(`{"free_weekend":{"end_time":%d}}`, ends.Unix())},
		inboxFixtureGame{firstApp + 6, 30, 100, ""},
		inboxFixtureGame{firstApp + 7, -1, 0, `{"release":{"is_preload":true,"is_coming_soon":false}}`},
	)
	got, changed := scanTestInbox(t, a, &english)
	if !changed || len(got) != 4 {
		t.Fatalf("expected a release, a demo and two giveaways, got %+v", got)
	}
	kinds := make(map[string]string)
	for _, thing := range got {
		kinds[thing.URL] = thing.Kind
	}
	for id, want := range map[uint32]string{
		firstApp: wishlistReleaseKind, secondApp: wishlistDemoKind, thirdApp: wishlistGiveawayKind, firstApp + 3: wishlistGiveawayKind,
	} {
		if kinds[storeLink(id)] != want {
			t.Fatalf("unexpected kind for a synthetic game, want %s in %+v", want, kinds)
		}
	}
	now = now.Add(time.Minute)
	if again, changed := scanTestInbox(t, a, &english); changed || !reflect.DeepEqual(got, again) {
		t.Fatalf("the same release, demo and giveaways replayed with new IDs: %+v", again)
	}
	now = ends
	if restored := a.collectWishlistInbox().Things; len(restored) != 3 {
		t.Fatalf("the expired free-to-keep offer must leave the collection: %+v", restored)
	}
}

func TestWishlistInboxSelectedTriggers(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	now := fixtureObservationAt
	a := newInboxTestApp(f, t.TempDir(), func() time.Time { return now }, config{
		TokenConfig: TokenConfig{InboxEventsEnabled: true}, WishlistEventTriggers: []string{}, WishlistTargetPrices: appIDText(firstApp) + ":USD:399",
	})
	seedInboxWishlist(t, f,
		inboxFixtureGame{firstApp, 399, 0, `{"is_coming_soon":true}`},
		inboxFixtureGame{secondApp, 300, 60, `{"related_items":{"demos":[{"appid":4000000199}]}}`},
		inboxFixtureGame{thirdApp, 0, 100, ""},
	)
	got, changed := scanTestInbox(t, a, &english)
	if !changed || len(got) != 1 || got[0].URL != storeLink(firstApp) {
		t.Fatalf("an empty trigger list should still allow a configured target: %+v", got)
	}
	contains(t, got[0].Preview, "Target price reached")
}

func TestWishlistInboxRoundup(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			f := newFakeSteam(t, time.Second)
			now := fixtureObservationAt
			cfg := config{TokenConfig: TokenConfig{InboxEventsEnabled: true, InboxMaxEventsPerDay: 20, SaleRoundupEnabled: enabled}}
			dir := t.TempDir()
			a := newInboxTestApp(f, dir, func() time.Time { return now }, cfg)
			requireStoreOK(t, a.stores.prices.Observe(fixtureID, "US", now.Add(-time.Hour), map[uint32]PriceQuote{
				firstApp: {Currency: "USD", Final: 500},
			}))
			var sales []inboxFixtureGame
			for i := range 6 {
				sales = append(sales, inboxFixtureGame{firstApp + uint32(i), 450 - 10*i, 50 + 5*i, ""})
			}
			sales[0].discount = 30
			sales = append(sales, inboxFixtureGame{firstApp + 6, 0, 100, ""})
			seedInboxWishlist(t, f, sales...)
			got, changed := scanTestInbox(t, a, &english)
			if !changed {
				t.Fatal("qualifying deals did not notify the inbox")
			}
			if !enabled {
				if len(got) != 7 || slices.ContainsFunc(got, func(e wire.ConnectorInboxThing) bool { return e.Kind == wishlistRoundupKind }) {
					t.Fatalf("roundup disabled should return individual alerts: %+v", got)
				}
				return
			}
			idx := slices.IndexFunc(got, func(e wire.ConnectorInboxThing) bool { return e.Kind == wishlistRoundupKind })
			if len(got) != 2 || idx < 0 || got[1-idx].Kind != wishlistGiveawayKind {
				t.Fatalf("expected one roundup and one giveaway: %+v", got)
			}
			roundup := got[idx]
			contains(t, roundup.Preview, "6 wishlist games", "Top 5")
			rows := strings.Split(roundup.Context, "\n")
			if len(rows) != 5 || !strings.Contains(rows[0], storeLink(firstApp)) || !strings.Contains(rows[1], storeLink(firstApp+5)) {
				t.Fatalf("roundup should rank the local low and then the deepest discount: %s", roundup.Context)
			}
			now = now.Add(wishlistPollInterval)
			if again, changed := scanTestInbox(t, a, &english); changed || !reflect.DeepEqual(got, again) {
				t.Fatalf("continuous sale produced another roundup: %+v", again)
			}
			cfg.SaleRoundupEnabled = false
			a = newInboxTestApp(f, dir, func() time.Time { return now }, cfg)
			if got, _ := scanTestInbox(t, a, &english); slices.ContainsFunc(got, func(e wire.ConnectorInboxThing) bool { return e.Kind == wishlistRoundupKind }) {
				t.Fatalf("disabled roundups resurfaced from a persisted snapshot: %+v", got)
			}
		})
	}
}

func TestWishlistInboxSaleWindowsSurviveRestart(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	now := fixtureObservationAt
	dir := t.TempDir()
	cfg := config{TokenConfig: TokenConfig{InboxEventsEnabled: true}, WishlistEventTriggers: []string{"discount"}}
	open := func() *app { return newInboxTestApp(f, dir, func() time.Time { return now }, cfg) }
	a := open()
	ends := now.Add(3 * time.Hour)
	set := func(final, discount int, until time.Time) {
		t.Helper()
		meta := ""
		if !until.IsZero() {
			meta = fmt.Sprintf(`{"best_purchase_option":{"final_price_in_cents":%q,"discount_pct":%d,"active_discounts":[{"discount_end_date":%d}]}}`,
				fmt.Sprint(final), discount, until.Unix())
		}
		seedInboxWishlist(t, f, inboxFixtureGame{firstApp, final, discount, meta})
	}
	set(299, 50, ends)
	first, changed := scanTestInbox(t, a, &english)
	if !changed || len(first) != 1 {
		t.Fatalf("expected a first sale, got %+v", first)
	}
	now = now.Add(time.Hour)
	a = open()
	set(299, 60, ends)
	if got, changed := scanTestInbox(t, a, &english); changed || !reflect.DeepEqual(got, first) {
		t.Fatalf("same price and window changed the event after restart: %+v", got)
	}
	now = now.Add(time.Hour)
	set(199, 70, ends)
	if got, changed := scanTestInbox(t, a, &english); !changed || len(got) != 2 {
		t.Fatalf("a new lower price in the window should emit once: %+v", got)
	}
	set(299, 50, ends)
	if got, changed := scanTestInbox(t, a, &english); changed || len(got) != 2 {
		t.Fatalf("the first price resurfaced inside the same window: %+v", got)
	}
	now = ends.Add(time.Minute)
	set(299, 50, now.Add(time.Hour))
	got, changed := scanTestInbox(t, a, &english)
	if !changed || len(got) != 1 || got[0].Thread == first[0].Thread {
		t.Fatalf("Steam's new sale window did not rearm the same price: %+v", got)
	}
	// Without Steam's end date, a verified regular price separates two offers.
	now = now.Add(2 * time.Hour)
	set(599, 0, time.Time{})
	if _, changed := scanTestInbox(t, a, &english); changed {
		t.Fatal("regular price created a discount alert")
	}
	now = now.Add(time.Minute)
	set(299, 50, time.Time{})
	if got, changed := scanTestInbox(t, a, &english); !changed || len(got) != 1 {
		t.Fatalf("a verified regular price did not rearm the locally observed sale window: %+v", got)
	}
}

func TestWishlistDeliveriesCountryBoundary(t *testing.T) {
	at := fixtureObservationAt
	entries := make(map[string]eventCursorRecord)
	for _, record := range []struct{ account, key, thread string }{
		{fixtureID, "US/offer", "same-country"},
		{fixtureID, "USA/offer", "longer-prefix"},
		{fixtureID, "USoffer", "missing-separator"},
		{fixtureID, "GB/offer", "different-country"},
		{fixtureID2, "US/offer", "different-account"},
	} {
		key, err := eventCursorKey(record.account, wishlistPriceKind, record.key, "fixture-window")
		requireStoreOK(t, err)
		entries[key] = eventCursorRecord{Delivery: &wishlistDelivery{
			Thing: wire.ConnectorInboxThing{Thread: record.thread},
			Until: at.Add(time.Hour),
		}}
	}
	got := wishlistDeliveries(entries, fixtureID, "US", at)
	if len(got) != 1 || got[0].Thing.Thread != "same-country" {
		t.Fatalf("US replay must match only its delimited country and account, got %+v", got)
	}
}

func TestWishlistInboxDailyBudgetConcurrent(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	var games []inboxFixtureGame
	for i := range 8 {
		games = append(games, inboxFixtureGame{firstApp + uint32(i), 300, 60, ""})
	}
	seedInboxWishlist(t, f, games...)
	dir := t.TempDir()
	at := fixtureObservationAt.Add(13 * time.Hour)
	open := func(country string, at time.Time) *app {
		return newInboxTestApp(f, dir, func() time.Time { return at }, config{
			CountryCode: country, TokenConfig: TokenConfig{InboxEventsEnabled: true, InboxMaxEventsPerDay: 3},
		})
	}
	var wg sync.WaitGroup
	var notified atomic.Int32
	for _, country := range []string{"US", "GB", "US", "GB"} {
		a := open(country, at)
		wg.Go(func() { a.scanWishlistInbox(t.Context(), &english, func() { notified.Add(1) }) })
	}
	wg.Wait()
	if notified.Load() != 1 {
		t.Fatalf("one scan should claim the daily budget, got %d notifications", notified.Load())
	}
	read := func(at time.Time) ([]wire.ConnectorInboxThing, bool) {
		t.Helper()
		a, aChanged := scanTestInbox(t, open("US", at), &english)
		b, bChanged := scanTestInbox(t, open("GB", at), &english)
		return append(a, b...), aChanged || bChanged
	}
	if got, changed := read(at.Add(10 * time.Minute)); changed || len(got) != 3 {
		t.Fatalf("same UTC day after restart must keep exactly three deliveries: %+v", got)
	}
	at = at.Add(time.Hour)
	got, changed := read(at)
	if !changed || len(got) != 6 {
		t.Fatalf("the next day should add three deliveries while replaying the first three: %+v", got)
	}
	n := 0
	for _, thing := range got {
		if thing.At.Equal(at) {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("the new day produced %d deliveries, want 3", n)
	}
}

func TestWishlistInboxDailyLimitAtConfiguredMaximum(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	var games []inboxFixtureGame
	for i := range 55 {
		games = append(games, inboxFixtureGame{firstApp + uint32(i), 350, 60, ""})
	}
	seedInboxWishlist(t, f, games...)
	at := fixtureObservationAt
	a := newInboxTestApp(f, t.TempDir(), func() time.Time { return at }, config{
		TokenConfig: TokenConfig{InboxEventsEnabled: true, InboxMaxEventsPerDay: 50},
	})
	if got, changed := scanTestInbox(t, a, &english); !changed || len(got) != 50 {
		t.Fatalf("configured maximum produced %d deliveries", len(got))
	}
	at = at.Add(time.Hour)
	if got, changed := scanTestInbox(t, a, &english); changed || len(got) != 50 {
		t.Fatalf("the remaining deals exceeded the daily cap: %d deliveries", len(got))
	}
}

func TestWishlistInboxReadsSliceFourCursors(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	seedInboxWishlist(t, f, inboxFixtureGame{firstApp, 300, 60, ""})
	dir := t.TempDir()
	now := fixtureObservationAt
	key, err := eventCursorKey(fixtureID, "sale", "fixture-legacy", "fixture-window")
	requireStoreOK(t, err)
	raw, err := json.Marshal(snapshotRecord[time.Time]{Version: storeVersion, Entries: map[string]time.Time{key: now.Add(-time.Hour)}})
	requireStoreOK(t, err)
	requireStoreOK(t, os.Mkdir(filepath.Join(dir, storeDirectory), 0o700))
	requireStoreOK(t, os.WriteFile(filepath.Join(dir, storeDirectory, "events.json"), raw, 0o600))
	a := newInboxTestApp(f, dir, func() time.Time { return now }, config{TokenConfig: TokenConfig{InboxEventsEnabled: true, InboxMaxEventsPerDay: 1}})
	if got, changed := scanTestInbox(t, a, &english); changed || len(got) != 0 {
		t.Fatalf("the legacy delivery must still count against today's budget: %+v", got)
	}
	now = now.AddDate(0, 0, 1)
	if got, changed := scanTestInbox(t, a, &english); !changed || len(got) != 1 {
		t.Fatalf("an existing Slice 4 cursor prevented a new delivery: %+v", got)
	}
}

func TestWishlistInboxCursorWriteFailure(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	seedInboxWishlist(t, f, inboxFixtureGame{firstApp, 200, 50, ""})
	dir := t.TempDir()
	a := newInboxTestApp(f, dir, func() time.Time { return fixtureObservationAt }, config{TokenConfig: TokenConfig{InboxEventsEnabled: true}})
	path := filepath.Join(dir, storeDirectory, "events.json")
	requireStoreOK(t, os.MkdirAll(path, 0o700))
	if got, changed := scanTestInbox(t, a, &english); len(got) != 0 || changed {
		t.Fatalf("a failed cursor write must prevent publication: %+v", got)
	}
	requireStoreOK(t, os.Remove(path))
	if got, changed := scanTestInbox(t, a, &english); len(got) != 1 || !changed {
		t.Fatalf("the same scan should retry after repair: %+v", got)
	}
}

func TestWishlistInboxQRLogoutDuringScan(t *testing.T) {
	now := time.Now()
	refresh := fixtureSecret(t, fixtureID, now).RefreshToken
	access := fixtureJWT(t, fixtureID, now.Add(time.Hour), "inbox-access")
	f := newFakeSteamMode(t, 2*time.Second, access)
	f.handle(authBasePath+beginQRPath+"/v1/", func(w http.ResponseWriter, _ *http.Request) { authFixture(w, "1", beginFixture()) })
	f.handle(authBasePath+pollQRPath+"/v1/", func(w http.ResponseWriter, _ *http.Request) {
		authFixture(w, "1", grantFixture(refresh, access))
	})
	seedInboxWishlist(t, f, inboxFixtureGame{firstApp, 200, 50, ""})
	dir := t.TempDir()
	a := &app{}
	h := startInboxTestHost(t, f, a, time.Now, dir, "en", `"auth_mode":"qr","inbox_events_enabled":true`)
	if got := collectWishlistInbox(t, h); len(got) != 0 || f.hit(wishlistPath) != 0 {
		t.Fatal("a signed-out account started the wishlist scanner")
	}
	authStep(t, h, "")
	await(t, 6*time.Second, func() bool { return hasEvent(h, "connector.changed") })
	if got := collectWishlistInbox(t, h); len(got) != 1 || got[0].Kind != wishlistPriceKind {
		t.Fatalf("QR sign-in did not activate the scanner: %+v", got)
	}
	a.qr.mu.Lock()
	session := a.qr.session
	a.qr.mu.Unlock()
	blocked := make(chan struct{})
	f.handle(storePricesPath, func(_ http.ResponseWriter, r *http.Request) {
		close(blocked)
		<-r.Context().Done()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		session.reads.scanWishlistInbox(session.ctx, &english, func() { t.Error("canceled QR scan notified the Inbox") })
	}()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("the concurrent scan did not reach Steam")
	}
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			var out wire.ConnectorCollectResult
			if err := h.Call("connector.inbox.collect", wire.ConnectorParams{Contribution: extensionID}, &out); err != nil {
				t.Error(err)
			}
		})
	}
	var status wire.AuthStatus
	if err := h.Call("auth.logout", struct{}{}, &status); err != nil || status.State != wire.AuthStateSignedOut {
		t.Fatalf("QR logout failed: %v, %s", err, status.State)
	}
	group.Wait()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("logout did not cancel the in-flight scan")
	}
	if got := collectWishlistInbox(t, h); len(got) != 0 || session.reads.inbox.Load() != nil || len(h.Secret(extensionID)) != 0 {
		t.Fatal("the signed-out QR generation kept its Inbox or credentials")
	}
	history, err := session.reads.stores.prices.Histories(fixtureID, "US", firstApp)
	if err != nil || len(history) != 0 {
		t.Fatal("the signed-out QR generation kept its price history")
	}
	requireStoreOK(t, session.reads.stores.cursors.file.view(func(entries map[string]eventCursorRecord) {
		if len(entries) != 0 {
			t.Fatal("the signed-out QR generation kept its event cursors")
		}
	}))
	for _, snapshotDir := range []string{"steam-cache-v1", storeDirectory} {
		if _, err := os.Lstat(filepath.Join(dir, snapshotDir)); !os.IsNotExist(err) {
			t.Fatalf("QR sign-in created %s on disk: %v", snapshotDir, err)
		}
	}
}

func TestWishlistDealsToolWithoutInbox(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	seedInboxWishlist(t, f,
		inboxFixtureGame{firstApp, 300, 20, ""},
		inboxFixtureGame{secondApp, -1, 0, `{"best_purchase_option":{"is_free_to_keep":true}}`},
		inboxFixtureGame{thirdApp, 700, 0, ""},
	)
	at := fixtureObservationAt
	requireStoreOK(t, NewPriceHistoryStore(dir).Observe(fixtureID, "US", at.Add(-time.Hour), map[uint32]PriceQuote{
		firstApp: {Currency: "USD", Final: 400},
	}))
	h := startInboxTestHost(t, f, &app{}, func() time.Time { return at }, dir, "en", `"inbox_events_enabled":false`)
	raw := toolAnswer(t, h, wishlistDealsName, `{"limit":1}`)
	contains(t, raw, `"matched_count":2`, `"truncated":true`, `"giveaway":true`, storeLink(secondApp))
	if strings.Contains(raw, storeLink(firstApp)) {
		t.Fatal("on-demand deals exceeded the requested limit")
	}
	raw = toolAnswer(t, h, wishlistDealsName, `{"limit":2}`)
	contains(t, raw, `"observed_new_low":true`, storeLink(firstApp))
	assertHidden(t, raw, fixtureID, f.key)
	if got := collectWishlistInbox(t, h); len(got) != 0 || hasEvent(h, "connector.changed") || f.hit(wishlistPath) != 2 {
		t.Fatal("on-demand deals produced background activity")
	}
}
