package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var fixtureObservationAt = time.Date(2026, time.January, 15, 10, 0, 0, 0, time.UTC)

func requireStoreOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func priceHistoryFor(t *testing.T, store *PriceHistoryStore, steamID, country string, appID uint32) PriceHistory {
	t.Helper()
	histories, err := store.Histories(steamID, country, appID)
	requireStoreOK(t, err)
	return histories[appID]
}

func TestPriceHistoryPersistsScopedLowsAndLatest(t *testing.T) {
	dir := t.TempDir()
	store := NewPriceHistoryStore(dir)
	at := fixtureObservationAt
	observe := func(id, country, currency string, final uint32, discount int, when time.Time) {
		t.Helper()
		requireStoreOK(t, store.Observe(id, country, when, map[uint32]PriceQuote{
			firstApp: {Currency: currency, Final: final, DiscountPercent: discount},
		}))
	}
	observe(fixtureID, " us ", "USD", 0, 100, at)
	observe(fixtureID, "US", "USD", 499, 20, at.Add(time.Hour))
	observe(fixtureID, "US", "USD", 0, 100, at.Add(24*time.Hour))
	observe(fixtureID, "US", "USD", 699, 0, at.Add(25*time.Hour))
	observe(fixtureID, "US", "USD", 399, 40, at.Add(24*time.Hour+time.Minute))
	observe(fixtureID, "GB", "GBP", 299, 70, at)
	observe(fixtureID2, "US", "USD", 349, 50, at)

	reopened := NewPriceHistoryStore(dir)
	h := priceHistoryFor(t, reopened, fixtureID, "US", firstApp)
	if len(h.Samples) != 2 || h.Samples[0].Final != 499 || h.Samples[1] != h.Latest || h.Latest.Final != 699 ||
		h.ObservedLowestFinal != 0 || h.ObservedLowestDiscount != 100 || !h.ObservedLowestAt.Equal(at) {
		t.Fatalf("wrong stored samples or first low: %+v", h)
	}
	if got := priceHistoryFor(t, reopened, fixtureID, "GB", firstApp).ObservedLowestFinal; got != 299 {
		t.Fatalf("different country has low %d", got)
	}
	if got := priceHistoryFor(t, reopened, fixtureID2, "US", firstApp).ObservedLowestFinal; got != 349 {
		t.Fatalf("different account has low %d", got)
	}

	observe(fixtureID, "US", "CAD", 999, 10, at.Add(48*time.Hour))
	observe(fixtureID, "US", "USD", 99, 90, at.Add(47*time.Hour))
	h = priceHistoryFor(t, reopened, fixtureID, "US", firstApp)
	if len(h.Samples) != 1 || h.Latest.Currency != "CAD" || h.ObservedLowestFinal != 999 {
		t.Fatalf("prices were compared across currencies: %+v", h)
	}
}

func TestPriceHistoryBoundsInMemory(t *testing.T) {
	store := NewPriceHistoryStore("")
	at := fixtureObservationAt
	prices := map[uint32]PriceQuote{firstApp: {Currency: "USD", Final: 99, DiscountPercent: 90}}
	requireStoreOK(t, store.Observe(fixtureID, "US", at, prices))
	prices[firstApp] = PriceQuote{Currency: "USD", Final: 199}
	for day := 1; day <= maxPriceSamples; day++ {
		requireStoreOK(t, store.Observe(fixtureID, "US", at.AddDate(0, 0, day), prices))
	}
	h := priceHistoryFor(t, store, fixtureID, "US", firstApp)
	if len(h.Samples) != maxPriceSamples || !h.Samples[0].At.Equal(at.AddDate(0, 0, 1)) || h.ObservedLowestFinal != 99 {
		t.Fatalf("sample cap discarded the low or kept excess samples: %+v", h)
	}
	h.Samples[0].Final = 0
	if got := priceHistoryFor(t, store, fixtureID, "US", firstApp).Samples[0].Final; got != 199 {
		t.Fatalf("mutating a returned sample changed the stored price to %d", got)
	}

	ids := make([]uint32, 0, maxPriceHistories+1)
	ids = append(ids, firstApp)
	for id := uint32(1); id <= maxPriceHistories; id++ {
		prices[id] = PriceQuote{Currency: "USD", Final: id}
		ids = append(ids, id)
	}
	delete(prices, firstApp)
	requireStoreOK(t, store.Observe(fixtureID, "US", at.AddDate(0, 0, maxPriceSamples+1), prices))
	histories, err := store.Histories(fixtureID, "US", ids...)
	requireStoreOK(t, err)
	if len(histories) != maxPriceHistories || !histories[firstApp].Latest.At.IsZero() {
		t.Fatal("price history cap did not evict the oldest series")
	}
}

func TestPlaytimeSnapshotDaysAndAccountScope(t *testing.T) {
	dir := t.TempDir()
	store := NewPlaytimeSnapshotStore(dir)
	at := fixtureObservationAt
	before := PlaytimeSnapshot{At: at, TotalMinutes: 300, TwoWeekMinutes: 45, OwnedGames: 6, UnplayedGames: 4}
	later := PlaytimeSnapshot{At: at.Add(time.Hour), TotalMinutes: 330, TwoWeekMinutes: 75, OwnedGames: 6, UnplayedGames: 3}
	requireStoreOK(t, store.Record(fixtureID, before))
	localLater := later
	localLater.At = later.At.In(time.FixedZone("fixture", 14*60*60))
	requireStoreOK(t, store.Record(fixtureID, localLater))
	requireStoreOK(t, store.Record(fixtureID, before))
	requireStoreOK(t, store.Record(fixtureID2, before))

	reopened := NewPlaytimeSnapshotStore(dir)
	got, err := reopened.History(fixtureID)
	requireStoreOK(t, err)
	if !reflect.DeepEqual(got, []PlaytimeSnapshot{later}) {
		t.Fatalf("same UTC day should keep the latest snapshot: %+v", got)
	}
	other, err := reopened.History(fixtureID2)
	requireStoreOK(t, err)
	if !reflect.DeepEqual(other, []PlaytimeSnapshot{before}) {
		t.Fatalf("different account has history %+v", other)
	}
}

func TestPlaytimeSnapshotBoundsInMemory(t *testing.T) {
	store := NewPlaytimeSnapshotStore("")
	at := fixtureObservationAt
	for day := 0; day <= maxPlaytimeDays; day++ {
		requireStoreOK(t, store.Record(fixtureID, PlaytimeSnapshot{At: at.AddDate(0, 0, day)}))
	}
	requireStoreOK(t, store.Record(fixtureID, PlaytimeSnapshot{At: at}))
	history, err := store.History(fixtureID)
	requireStoreOK(t, err)
	if len(history) != maxPlaytimeDays || !history[0].At.Equal(at.AddDate(0, 0, 1)) {
		t.Fatalf("unexpected retained history, %d days", len(history))
	}
	for i := 1; i <= maxPlaytimeAccounts; i++ {
		id := strconv.FormatUint(steamIDBase+(1<<32)-1-uint64(i), 10)
		requireStoreOK(t, store.Record(id, PlaytimeSnapshot{At: at.AddDate(0, 0, maxPlaytimeDays+i)}))
	}
	history, err = store.History(fixtureID)
	requireStoreOK(t, err)
	if len(history) != 0 {
		t.Fatal("playtime account cap did not evict the oldest history")
	}
}

func TestBacklogPersistsStatusNotesAndTags(t *testing.T) {
	dir := t.TempDir()
	store := NewBacklogStore(dir)
	for i, status := range []BacklogStatus{BacklogUnplayed, BacklogPlaying, BacklogQueued, BacklogPaused, BacklogCompleted, BacklogDropped} {
		requireStoreOK(t, store.Set(fixtureID, firstApp+uint32(i), BacklogEntry{Status: status}))
	}
	entry := BacklogEntry{Status: BacklogPaused, Note: "合成备注", Tags: []string{"fixture", "单人"}}
	requireStoreOK(t, store.Set(fixtureID, firstApp, entry))
	requireStoreOK(t, store.Set(fixtureID2, firstApp, BacklogEntry{Status: BacklogDropped}))

	reopened := NewBacklogStore(dir)
	entries, err := reopened.List(fixtureID)
	requireStoreOK(t, err)
	if len(entries) != 6 || !reflect.DeepEqual(entries[firstApp], entry) || entries[firstApp+4].Status != BacklogCompleted {
		t.Fatalf("unexpected backlog entries %+v", entries)
	}
	requireStoreOK(t, reopened.Delete(fixtureID, firstApp))
	if _, ok, err := store.Get(fixtureID, firstApp); err != nil || ok {
		t.Fatalf("deleted entry still present: found=%t, err=%v", ok, err)
	}
	if got, ok, err := store.Get(fixtureID2, firstApp); err != nil || !ok || got.Status != BacklogDropped {
		t.Fatalf("another account's label changed: %+v, found=%t, err=%v", got, ok, err)
	}
}

func TestBacklogBoundsAndCopiesInMemory(t *testing.T) {
	store := NewBacklogStore("")
	entry := BacklogEntry{Status: BacklogQueued, Note: strings.Repeat("游", maxBacklogNoteRunes), Tags: []string{strings.Repeat("游", maxBacklogTagRunes)}}
	requireStoreOK(t, store.Set(fixtureID, firstApp, entry))
	entry.Tags[0] = "changed"
	got, ok, err := store.Get(fixtureID, firstApp)
	requireStoreOK(t, err)
	if !ok || got.Tags[0] == entry.Tags[0] {
		t.Fatal("the store retained the caller's tag slice")
	}
	got.Tags[0] = "changed"
	for _, invalid := range []BacklogEntry{
		{Status: "other"},
		{Status: BacklogPlaying, Note: entry.Note + "x"},
		{Status: BacklogPlaying, Tags: []string{strings.Repeat("x", maxBacklogTagRunes+1)}},
		{Status: BacklogPlaying, Tags: make([]string, maxBacklogTags+1)},
	} {
		if err := store.Set(fixtureID, firstApp, invalid); err == nil {
			t.Fatalf("accepted invalid annotation %+v", invalid)
		}
	}
	got, ok, err = store.Get(fixtureID, firstApp)
	requireStoreOK(t, err)
	if !ok || got.Status != BacklogQueued || got.Tags[0] == "changed" {
		t.Fatalf("invalid writes or tag mutation changed the stored entry: %+v", got)
	}
	for id := uint32(1); id < maxBacklogEntries; id++ {
		requireStoreOK(t, store.Set(fixtureID, id, BacklogEntry{Status: BacklogUnplayed}))
	}
	if err := store.Set(fixtureID, secondApp, entry); err == nil {
		t.Fatal("store accepted an insert past its capacity")
	}
	requireStoreOK(t, store.Set(fixtureID, firstApp, BacklogEntry{Status: BacklogCompleted}))
}

func TestEventCursorsPersistByWindowAndExpire(t *testing.T) {
	dir := t.TempDir()
	store := NewEventCursorStore(dir)
	at := fixtureObservationAt
	first := EventCursor{Window: "fixture-sale-1", SentAt: at}
	second := EventCursor{Window: "fixture-sale-2", SentAt: at.Add(time.Hour)}
	requireStoreOK(t, store.MarkSent(fixtureID, "sale", "fixture-app", first))
	requireStoreOK(t, store.MarkSent(fixtureID, "sale", "fixture-app", second))

	reopened := NewEventCursorStore(dir)
	for _, want := range []EventCursor{first, second} {
		got, seen, err := reopened.Lookup(fixtureID, "sale", "fixture-app", want.Window, second.SentAt)
		if err != nil || !seen || got != want {
			t.Fatalf("window %q cursor %+v, seen=%t, err=%v", want.Window, got, seen, err)
		}
	}
	for _, key := range [][4]string{
		{fixtureID2, "sale", "fixture-app", first.Window},
		{fixtureID, "patch", "fixture-app", first.Window},
		{fixtureID, "sale", "other-app", first.Window},
		{fixtureID, "sale", "fixture-app", "new-window"},
	} {
		if _, seen, err := reopened.Lookup(key[0], key[1], key[2], key[3], second.SentAt); err != nil || seen {
			t.Fatalf("unrelated cursor %+v: seen=%t, err=%v", key, seen, err)
		}
	}
	expiry := at.Add(eventCursorTTL)
	if _, seen, err := reopened.Lookup(fixtureID, "sale", "fixture-app", first.Window, expiry); err != nil || seen {
		t.Fatalf("expired cursor: seen=%t, err=%v", seen, err)
	}
	requireStoreOK(t, reopened.Prune(expiry))
	if _, seen, err := store.Lookup(fixtureID, "sale", "fixture-app", first.Window, second.SentAt); err != nil || seen {
		t.Fatalf("expired entry was not removed: seen=%t, err=%v", seen, err)
	}
	if _, seen, err := store.Lookup(fixtureID, "sale", "fixture-app", second.Window, expiry); err != nil || !seen {
		t.Fatalf("prune removed an active window: seen=%t, err=%v", seen, err)
	}
}

func TestEventCursorCapacityInMemory(t *testing.T) {
	store := NewEventCursorStore("")
	at := fixtureObservationAt
	for n := 0; n <= maxEventCursors; n++ {
		cursor := EventCursor{SentAt: at.Add(time.Duration(n) * time.Second)}
		requireStoreOK(t, store.MarkSent(fixtureID, "patch", strconv.Itoa(n), cursor))
	}
	if _, seen, err := store.Lookup(fixtureID, "patch", "0", "", at.Add(time.Hour)); err != nil || seen {
		t.Fatalf("oldest cursor at capacity: seen=%t, err=%v", seen, err)
	}
	if _, seen, err := store.Lookup(fixtureID, "patch", "1", "", at.Add(time.Hour)); err != nil || !seen {
		t.Fatalf("newer cursor at capacity: seen=%t, err=%v", seen, err)
	}
}

func TestSnapshotStoreRefusesUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "oversize", "malformed", "unknown-version"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			store := NewBacklogStore(dir)
			path := filepath.Join(dir, "steam-store-v1", "backlog.json")
			requireStoreOK(t, os.Mkdir(filepath.Dir(path), 0o700))
			var content string
			switch kind {
			case "symlink":
				target := filepath.Join(dir, "sentinel")
				requireStoreOK(t, os.WriteFile(target, []byte(`{"version":1,"entries":{}}`), 0o600))
				requireStoreOK(t, os.Symlink(target, path))
			case "directory":
				requireStoreOK(t, os.Mkdir(path, 0o700))
			case "oversize":
				requireStoreOK(t, os.WriteFile(path, nil, 0o600))
				requireStoreOK(t, os.Truncate(path, cacheMaxSize+1))
			case "malformed":
				content = `{"version":1,"entries":`
			case "unknown-version":
				content = `{"version":2,"entries":{}}`
			}
			if content != "" {
				requireStoreOK(t, os.WriteFile(path, []byte(content), 0o600))
			}
			before, err := os.Lstat(path)
			requireStoreOK(t, err)
			if _, _, err := store.Get(fixtureID, firstApp); err == nil {
				t.Fatal("read accepted an unsafe snapshot file")
			}
			if err := store.Set(fixtureID, firstApp, BacklogEntry{Status: BacklogQueued}); err == nil {
				t.Fatal("write accepted an unsafe snapshot file")
			}
			after, err := os.Lstat(path)
			requireStoreOK(t, err)
			if !os.SameFile(before, after) || before.Size() != after.Size() {
				t.Fatal("refused snapshot file was overwritten")
			}
			if content != "" {
				raw, err := os.ReadFile(path)
				requireStoreOK(t, err)
				if string(raw) != content {
					t.Fatal("refused snapshot contents changed")
				}
			}
		})
	}
}

func TestSnapshotStorePreservesPreviousRecordOnOversizeWrite(t *testing.T) {
	file := newSnapshotFile[string](t.TempDir(), "fixture")
	requireStoreOK(t, file.update(func(entries map[string]string) (bool, error) {
		entries["fixture"] = "original"
		return true, nil
	}))
	err := file.update(func(entries map[string]string) (bool, error) {
		entries["fixture"] = strings.Repeat("&", cacheMaxSize/6+1) // JSON escapes & as six bytes.
		return true, nil
	})
	if err == nil {
		t.Fatal("oversized snapshot was accepted")
	}
	requireStoreOK(t, file.view(func(entries map[string]string) {
		if entries["fixture"] != "original" {
			t.Fatal("failed write changed the previous snapshot")
		}
	}))
}

func TestSnapshotStoreAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	store := NewBacklogStore(dir)
	requireStoreOK(t, store.Set(fixtureID, firstApp, BacklogEntry{Status: BacklogQueued}))
	path := filepath.Join(dir, "steam-store-v1", "backlog.json")
	openBefore, err := os.Open(path)
	requireStoreOK(t, err)
	defer openBefore.Close()

	requireStoreOK(t, store.Set(fixtureID, firstApp, BacklogEntry{Status: BacklogPlaying}))
	before, err := io.ReadAll(openBefore)
	requireStoreOK(t, err)
	after, err := os.ReadFile(path)
	requireStoreOK(t, err)
	info, err := os.Stat(path)
	requireStoreOK(t, err)
	if !strings.Contains(string(before), `"status":"queued"`) || !strings.Contains(string(after), `"status":"playing"`) || info.Mode().Perm() != 0o600 {
		t.Fatal("replacement did not preserve the open snapshot or private file mode")
	}
}

func TestSnapshotStoreConcurrentHandles(t *testing.T) {
	dir := t.TempDir()
	stores := []*BacklogStore{NewBacklogStore(dir), NewBacklogStore(dir)}
	const writers = 32
	var all sync.WaitGroup
	for id := uint32(1); id <= writers; id++ {
		all.Go(func() {
			store := stores[int(id)%len(stores)]
			if err := store.Set(fixtureID, id, BacklogEntry{Status: BacklogPlaying}); err != nil {
				t.Error(err)
			}
			if _, ok, err := store.Get(fixtureID, id); err != nil || !ok {
				t.Errorf("concurrent read of app %d: found=%t, err=%v", id, ok, err)
			}
		})
	}
	all.Wait()
	entries, err := NewBacklogStore(dir).List(fixtureID)
	requireStoreOK(t, err)
	if len(entries) != writers {
		t.Fatalf("concurrent updates kept %d of %d entries", len(entries), writers)
	}
}

func TestSteamReadsRecordFreshSnapshots(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	now := fixtureObservationAt
	d := f.data(dir, fixtureID, "US", func() time.Time { return now })
	f.reply(ownedPath, http.StatusOK, fmt.Sprintf(`{"response":{"game_count":3,"games":[{"appid":%d,"playtime_forever":230,"playtime_2weeks":20},{"appid":%d,"playtime_forever":70,"playtime_2weeks":30},{"appid":%d}]}}`, firstApp, secondApp, thirdApp))
	p := d.account(t.Context(), profileTTL, false)
	if got := d.ownedGames(t.Context(), p, false); got.Issue != "" || got.Data == nil {
		t.Fatalf("owned games read failed: %+v", got)
	}
	if got := d.wishlist(t.Context(), p, "english", false); got.Issue != "" || got.Data == nil {
		t.Fatalf("wishlist read failed: %+v", got)
	}
	prices := NewPriceHistoryStore(dir)
	playtime := NewPlaytimeSnapshotStore(dir)
	want := PlaytimeSnapshot{At: now, TotalMinutes: 300, TwoWeekMinutes: 50, OwnedGames: 3, UnplayedGames: 1}
	checkPlaytime := func() {
		t.Helper()
		got, err := playtime.History(fixtureID)
		requireStoreOK(t, err)
		if !reflect.DeepEqual(got, []PlaytimeSnapshot{want}) {
			t.Fatalf("saved playtime %+v, want %+v", got, want)
		}
	}
	checkPlaytime()
	checkPrice := func(appID, final uint32, at time.Time) {
		t.Helper()
		if got := priceHistoryFor(t, prices, fixtureID, "US", appID); got.Latest.Final != final || !got.Latest.At.Equal(at) {
			t.Fatalf("saved price for %d: %+v", appID, got)
		}
	}
	checkPrice(firstApp, 399, now)
	checkPrice(secondApp, 699, now)
	if got := priceHistoryFor(t, prices, fixtureID, "US", thirdApp); !got.Latest.At.IsZero() {
		t.Fatalf("an unpriced game was stored as a price observation: %+v", got)
	}

	now = now.Add(time.Minute)
	d.ownedGames(t.Context(), p, false)
	d.wishlist(t.Context(), p, "english", false)
	for _, path := range []string{ownedPath, wishlistPath, storePricesPath, storeNamesPath} {
		if hits := f.hit(path); hits != 1 {
			t.Fatalf("%s received %d requests for one fresh read and one cache hit", path, hits)
		}
	}
	if f.hit(recentPath) != 0 {
		t.Fatal("recording library playtime made an extra recent-games request")
	}
	checkPlaytime()
	checkPrice(firstApp, 399, fixtureObservationAt)

	f.reply(ownedPath, http.StatusTooManyRequests, "fixture rate limit")
	f.reply(storePricesPath, http.StatusServiceUnavailable, "fixture Store outage")
	if got := d.ownedGames(t.Context(), p, true); !got.Stale || got.Issue != problemRateLimited {
		t.Fatalf("expected library fallback: %+v", got)
	}
	if got := d.wishlist(t.Context(), p, "english", true); !got.Stale || got.Issue != problemNetwork {
		t.Fatalf("expected wishlist fallback: %+v", got)
	}
	checkPlaytime()
	checkPrice(firstApp, 399, fixtureObservationAt)

	now = now.Add(time.Minute)
	f.reply(ownedPath, http.StatusOK, fmt.Sprintf(`{"response":{"game_count":3,"games":[{"appid":%d,"playtime_forever":250}]}}`, firstApp))
	f.reply(storePricesPath, http.StatusOK, fmt.Sprintf(`{"%d":{"success":true,"data":{"price_overview":{"currency":"USD","final":199,"discount_percent":80}}},"%d":{"success":true,"data":{"price_overview":{"currency":"USD","final":1,"discount_percent":101}}},"%d":{"success":false}}`, firstApp, secondApp, thirdApp))
	if got := d.ownedGames(t.Context(), p, true); got.Issue != "" || got.Data == nil || !got.Data.Truncated {
		t.Fatalf("expected a readable but incomplete library: %+v", got)
	}
	if got := d.wishlist(t.Context(), p, "english", true); !got.Stale || got.Issue != problemResponse {
		t.Fatalf("expected a fallback after the partial Store response: %+v", got)
	}
	checkPlaytime()
	checkPrice(firstApp, 199, now)
	checkPrice(secondApp, 699, fixtureObservationAt)
}

func TestSteamSnapshotWriteFailureLeavesReadsAvailable(t *testing.T) {
	f := newFakeSteam(t, time.Second)
	dir := t.TempDir()
	requireStoreOK(t, os.WriteFile(filepath.Join(dir, storeDirectory), []byte("fixture blocking the store directory"), 0o600))
	d := f.data(dir, fixtureID, "US", func() time.Time { return fixtureObservationAt })
	p := d.account(t.Context(), profileTTL, false)
	if got := d.ownedGames(t.Context(), p, false); got.Issue != "" || got.Data == nil || got.Data.Total != 3 {
		t.Fatalf("local write failure affected the owned games result: %+v", got)
	}
	if got := d.wishlist(t.Context(), p, "english", false); got.Issue != "" || got.Data == nil || got.Data.Checked != 3 {
		t.Fatalf("local write failure affected the wishlist result: %+v", got)
	}
}
