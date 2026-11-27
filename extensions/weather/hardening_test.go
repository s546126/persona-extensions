package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestTruncatedAndOversizedJSONNeverPoisonCache(t *testing.T) {
	f := newFakeSite(t)
	sample(t, f)
	// Truncated JSON object starting with '{' must not be stored in cache.
	f.serve("/v1/forecast", `{"latitude":31.22,"current":{"temperature_2m":11`)
	a := testApp(t, f, "")

	if _, err := callTool(t, a, "weather_now", ""); err == nil {
		t.Fatal("expected error for truncated forecast JSON")
	}
	if hits := f.hit("/v1/forecast"); hits != 1 {
		t.Fatalf("first call hit forecast %d times, want 1", hits)
	}

	// Next call (without refresh!) should not receive a poisoned cache entry.
	f.serve("/v1/forecast", readFixture(t, "forecast.json"))
	out, err := callTool(t, a, "weather_now", "")
	if err != nil || !strings.Contains(out, "上海") {
		t.Fatalf("recovered read failed (cache poisoned?): out=%q err=%v", out, err)
	}
	if hits := f.hit("/v1/forecast"); hits != 2 {
		t.Fatalf("expected 2 forecast hits after truncated response, got %d", hits)
	}

	// Oversized response (> answerMost) must be rejected instead of silently truncated.
	g := newFakeSite(t)
	sample(t, g)
	oversized := `{"pad":"` + strings.Repeat("a", int(answerMost)+1024) + `"}`
	g.serve("/v1/forecast", oversized)
	b := testApp(t, g, "")
	if _, err := callTool(t, b, "weather_now", ""); err == nil {
		t.Fatal("expected error for oversized forecast body")
	}

	// Cache size stays bounded when many unique keys are stored.
	c := newClient()
	now := time.Now()
	for i := range 300 {
		c.store(fmt.Sprintf("https://api.open-meteo.com/k/%d", i), []byte(`{}`), now.Add(time.Duration(i)*time.Millisecond))
	}
	c.mu.Lock()
	n := len(c.cache)
	c.mu.Unlock()
	if n > 128 {
		t.Fatalf("cache grew to %d entries, want <= 128", n)
	}
}

func TestCachedReadDoesNotResurrectResolvedStatusError(t *testing.T) {
	f := newFakeSite(t)
	sample(t, f)
	a := testApp(t, f, `{"cities":[{"name":"上海","latitude":31.22,"longitude":121.46},{"name":"北京","latitude":39.9,"longitude":116.4}]}`)

	// 1. Read Shanghai at T0 (cached).
	if _, err := callTool(t, a, "weather_now", `{"city":"上海"}`); err != nil {
		t.Fatalf("shanghai initial: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	// 2. Fail Beijing at T1 -> status becomes error.
	f.put("/v1/forecast", answer{status: 503, body: "down"})
	if _, err := callTool(t, a, "weather_now", `{"city":"北京"}`); err == nil {
		t.Fatal("expected 503 for beijing")
	}
	if st := a.status(); st.State != "error" {
		t.Fatalf("expected error status after 503, got %+v", st)
	}
	time.Sleep(5 * time.Millisecond)

	// 3. Recover Beijing at T2 -> status returns to ok.
	f.serve("/v1/forecast", readFixture(t, "forecast.json"))
	if _, err := callTool(t, a, "weather_now", `{"city":"北京"}`); err != nil {
		t.Fatalf("beijing recovery: %v", err)
	}
	if st := a.status(); st.State != "ok" {
		t.Fatalf("expected ok status after recovery, got %+v", st)
	}

	// 4. Read cached Shanghai (timestamp T0 < T1) -> must NOT rewind lastOK and resurrect the T1 error!
	if _, err := callTool(t, a, "weather_now", `{"city":"上海"}`); err != nil {
		t.Fatalf("shanghai cached read: %v", err)
	}
	if st := a.status(); st.State != "ok" {
		t.Fatalf("cached read resurrected stale error: %+v", st)
	}
}

func TestLongCityNamesMissingDailyDateAndClipBounds(t *testing.T) {
	// clip(s, n) must return at most n runes.
	clipped := clip(strings.Repeat("字", 2500), wire.StringMost)
	if got := utf8.RuneCountInString(clipped); got > wire.StringMost {
		t.Fatalf("clip returned %d runes, want <= %d", got, wire.StringMost)
	}

	f := newFakeSite(t)
	sample(t, f)
	// Daily entry with invalid date must not format zero time as "01-01".
	f.serve("/v1/forecast", `{
		"timezone": "Asia/Shanghai",
		"current": {"time":"2026-11-09T17:00","temperature_2m":11,"apparent_temperature":9,"relative_humidity_2m":60,"weather_code":1,"wind_speed_10m":10,"wind_direction_10m":90},
		"hourly": {"time":["2026-11-09T17:00"],"temperature_2m":[11]},
		"daily": {"time":["","2026-11-10"],"weather_code":[1,2],"temperature_2m_max":[15,16],"temperature_2m_min":[8,9],"precipitation_probability_max":[20,10]}
	}`)
	longCity := strings.Repeat("超长城市名", 500) // 2500 runes > wire.StringMost
	cfg, _ := json.Marshal(map[string]any{
		"cities": []any{
			map[string]any{"name": longCity, "latitude": 31.22, "longitude": 121.46},
			map[string]any{"name": "北京", "latitude": 39.9, "longitude": 116.4},
		},
	})
	a := testApp(t, f, string(cfg))
	v := draw(t, a, 6, nil)
	saysNot(t, v, "01-01")

	// Tapping the long city row by index still selects it cleanly and stays valid under CheckView.
	v = tap(t, a, 6, v, "city:1", map[string]string{"city": "北京"})
	v = tap(t, a, 6, v, "city:0", nil)
	checked(t, v)
}

func TestConcurrentInitAndReadsNoRace(t *testing.T) {
	f := newFakeSite(t)
	sample(t, f)
	a := testApp(t, f, "")
	in := &ext.Init{InitializeParams: wire.InitializeParams{
		Config: json.RawMessage(`{"cities":["上海","北京 39.9,116.4"],"temperature_unit":"celsius","wind_unit":"kmh"}`),
	}}

	ctx := context.Background()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				_, _ = a.init(ctx, in, nil)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				_, _ = a.render(ctx, req(6, nil))
				_, _ = a.callTool(ctx, "weather_now", nil)
			}
		}()
	}
	wg.Wait()
}
