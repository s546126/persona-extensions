package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestBodyweightChartWorkoutEntriesRoutinesAndLongFieldsFitCheckView(t *testing.T) {
	f := newFakeGym(t)
	longName := strings.Repeat("超长名称", 700) // 2800 runes > wire.StringMost
	longID := "w_" + strings.Repeat("x", 150)

	f.bump(func(st map[string]any) {
		// 150 weigh-ins within 90 days (> wire.PointsMost = 120).
		bw := make([]any, 0, 150)
		for i := range 150 {
			d := time.Date(2026, 9, 1, 0, 0, 0, 0, zone).AddDate(0, 0, i/2).Format(time.DateOnly)
			bw = append(bw, map[string]any{"d": d, "w": 75.0 + float64(i%5)*0.1, "t": ms(d, "07:00")})
		}
		st["bodyweight"] = bw

		// 310 routines + coach week with many items + long routine/label names.
		routines := make([]any, 0, 310)
		qIDs := make([]any, 0, 80)
		for i := range 310 {
			id := fmt.Sprintf("r_%03d", i)
			name := fmt.Sprintf("计划 %d", i)
			if i == 0 {
				name = longName
			}
			routines = append(routines, map[string]any{
				"id": id, "name": name, "emoji": "figureStrength",
				"ex": []any{map[string]any{"id": "c_long", "sets": 3, "reps": 8, "weight": 80, "mode": "reps"}},
			})
			if i < 80 {
				qIDs = append(qIDs, id)
			}
		}
		st["routines"] = routines
		st["customEx"] = []any{map[string]any{"id": "c_long", "n": longName, "bp": "chest", "custom": true}}
		st["queue"] = map[string]any{"ids": qIDs, "startsOn": "2026-11-10", "label": longName}
		st["dayPlan"] = map[string]any{"2026-11-13": "r_000"}

		// Workout with > 300 entries, long ID (> 128 bytes), long name, many PRs.
		entries := make([]any, 0, 310)
		prs := make([]any, 0, 10)
		for i := range 310 {
			entries = append(entries, map[string]any{
				"id":   "c_long",
				"sets": []any{map[string]any{"w": 80, "r": 5, "done": true}},
			})
			if i < 10 {
				prs = append(prs, "c_long")
			}
		}
		st["workouts"] = []any{map[string]any{
			"id": longID, "d": "2026-11-12", "start": ms("2026-11-12", "18:00"), "end": ms("2026-11-12", "19:00"),
			"name": longName, "prs": prs, "entries": entries,
		}}
	})

	a := testApp(t, f, "")
	ctx := context.Background()

	// Overview page (bodyweight chart <= 120 points, long Action.ID handled, long names clipped).
	ov, err := a.renderPage(ctx, uiRequest(pageUI, 6, "", "", nil))
	if err != nil {
		t.Fatalf("overview render: %v", err)
	}
	checkView(t, ov, false)

	// Plan tab (310 routines + 80 coachWeek items + long names must stay <= wire.ItemsMost and <= wire.StringMost).
	planRes, err := a.pageAction(ctx, uiRequest(pageUI, 6, "tab", ov.State, map[string]string{"tab": tabPlan}))
	if err != nil {
		t.Fatalf("plan tab: %v", err)
	}
	checkView(t, planRes.View, false)

	// Workout detail (310 entries + long title + long PR list + long ID).
	detailRes, err := a.pageAction(ctx, uiRequest(pageUI, 6, "workout", ov.State, map[string]string{"id": longID}))
	if err != nil {
		t.Fatalf("workout detail: %v", err)
	}
	checkView(t, detailRes.View, false)

	// Overview card with long routine name.
	card, err := a.renderCard(ctx, uiRequest(cardUI, 6, "", "", nil))
	if err != nil {
		t.Fatalf("renderCard: %v", err)
	}
	checkView(t, card, true)

	// Long sync error must not crash CheckView when no snapshot exists.
	b := newApp(t.Logf)
	b.configure(config{ServerURL: f.url()}, fakeToken)
	errView := b.page(uiRequest(pageUI, 6, "", "", nil), pageState{}, errors.New(strings.Repeat("错误详情", 1000)))
	checkView(t, errView, false)

	// Extreme / non-finite floats must not overflow int64 in num().
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e25} {
		got := num(v)
		if strings.Contains(got, "-9,223,372,036,854,775,808") {
			t.Fatalf("num(%v) overflowed int64: %q", v, got)
		}
	}
}

func TestSummaryBodyweightDateUsesLocalZone(t *testing.T) {
	f := newFakeGym(t)
	f.bump(func(st map[string]any) {
		st["bodyweight"] = []any{
			map[string]any{"d": "2026-10-14", "w": 80.0, "t": ms("2026-10-14", "07:00")},
			map[string]any{"d": "2026-10-15", "w": 79.0, "t": ms("2026-10-15", "07:00")},
			map[string]any{"d": "2026-11-14", "w": 77.0, "t": ms("2026-11-14", "01:00")},
		}
	})
	a := testApp(t, f, "")
	// 2026-11-13 18:00 UTC is 2026-11-14 02:00 UTC+8; 30 days before local date 2026-11-14 is 2026-10-15 (79 kg),
	// NOT 2026-10-14 (80 kg).
	a.now = func() time.Time { return time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC) }
	text := a.summaryText(a.snapshot(), zone)
	if !strings.Contains(text, "30 天前 79 kg") {
		t.Fatalf("summaryText used UTC date instead of local zone:\n%s", text)
	}
}

func TestWriteIdempotencyScopedPerAppRejectsPayloadMismatchAndNoZoneRace(t *testing.T) {
	f1 := newFakeGym(t)
	a1 := testApp(t, f1, "")
	f2 := newFakeGym(t)
	a2 := testApp(t, f2, "")

	args1 := map[string]any{"date": "2026-11-18", "rest": true}
	args2 := map[string]any{"date": "2026-11-19", "rest": true}

	if _, err := call(t, a1, "actions.do", actScheduleDay, args1, "req-shared"); err != nil {
		t.Fatalf("a1 write: %v", err)
	}
	// Reusing the same request_id on a1 with DIFFERENT arguments must fail, not return a stale receipt.
	if out, err := call(t, a1, "actions.do", actScheduleDay, args2, "req-shared"); err == nil {
		t.Fatalf("reused request_id with different args succeeded silently: %s", out)
	}
	// A separate app instance a2 with the same request_id must execute its own write on f2.
	if _, err := call(t, a2, "actions.do", actScheduleDay, args1, "req-shared"); err != nil {
		t.Fatalf("a2 write: %v", err)
	}
	if f2.nPuts() != 1 {
		t.Fatalf("a2 skipped write because of global done map leak from a1 (f2 puts=%d)", f2.nPuts())
	}

	// Concurrent init + renderPage / callTool must not race on a.zone.
	in := &ext.Init{InitializeParams: wire.InitializeParams{
		Config:  json.RawMessage(fmt.Sprintf(`{"server_url":%q,"exercise_names":false}`, f1.url())),
		Secrets: map[string]string{secretToken: fakeToken},
		Hub:     wire.HubInfo{Timezone: "Asia/Shanghai"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_, _ = a1.init(ctx, in, nil)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_, _ = a1.renderPage(ctx, uiRequest(pageUI, 6, "", "", nil))
				_, _ = call(t, a1, "tools.call", toolSummary, nil, "")
			}
		}()
	}
	wg.Wait()
	// Stop poll goroutine cleanly before test server closes.
	a1.mu.Lock()
	if a1.cancel != nil {
		a1.cancel()
	}
	a1.mu.Unlock()
}
