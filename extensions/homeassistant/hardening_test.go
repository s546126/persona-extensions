package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHardeningOversizedAreaStatePassesCheckView(t *testing.T) {
	a := newApp(func(string, ...any) {})
	huge := strings.Repeat("a", 20000)

	// 1. Render when unconfigured (error view echoes st into v.State).
	v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Area: huge}),
	}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("render with oversized Area failed wire.CheckView: %v", err)
	}

	// 2. Action "area" / "filter" / "tab:room:..." with oversized Area.
	for _, req := range []*ext.UIRequest{
		{UIParams: wire.UIParams{UIVersion: 6, Action: "area", Values: map[string]string{"area": huge}}},
		{UIParams: wire.UIParams{UIVersion: 6, Action: "filter", Values: map[string]string{"area": huge}}},
		{UIParams: wire.UIParams{UIVersion: 6, Action: "tab", Values: map[string]string{"tab": roomTab + huge}}},
	} {
		res, err := a.action(context.Background(), req)
		if err != nil {
			t.Fatalf("action %s: %v", req.Action, err)
		}
		if err := wire.CheckView(res.View); err != nil {
			t.Fatalf("action %s with oversized Area failed wire.CheckView: %v", req.Action, err)
		}
	}
}

func TestHardeningClimateStateOutsideHVACModesPassesCheckView(t *testing.T) {
	f := newFakeHA(t)
	f.mu.Lock()
	ac := f.state["climate.living_ac"]
	ac.State = "idle" // Active state not listed in hvac_modes ["off", "cool", "heat", "auto"]
	f.state["climate.living_ac"] = ac
	f.mu.Unlock()

	a := newApp(func(string, ...any) {})
	a.configure(Config{URL: f.srv.URL}, f.token)

	v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Entity: "climate.living_ac"}),
	}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("climate entity with state outside hvac_modes failed wire.CheckView: %v", err)
	}
}

func TestHardeningHistoryLongNameAndUnitPassesCheckView(t *testing.T) {
	f := newFakeHA(t)
	longName := strings.Repeat("温", 60)
	longUnit := strings.Repeat("°C/m³·hr", 8)
	f.mu.Lock()
	f.state["sensor.long_temp"] = State{
		EntityID: "sensor.long_temp",
		State:    "24.5",
		Attributes: map[string]any{
			"friendly_name":       longName,
			"unit_of_measurement": longUnit,
			"device_class":        "temperature",
		},
		LastChanged: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	f.mu.Unlock()
	f.more = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/api/history/period/") {
			base := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			rows := [][]Change{{
				{State: "22.1", At: base},
				{State: "23.2", At: base.Add(time.Hour)},
				{State: "24.3", At: base.Add(2 * time.Hour)},
				{State: "24.5", At: base.Add(3 * time.Hour)},
			}}
			_ = json.NewEncoder(w).Encode(rows)
			return true
		}
		return false
	}

	a := newApp(func(string, ...any) {})
	a.configure(Config{URL: f.srv.URL}, f.token)

	v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Entity: "sensor.long_temp"}),
	}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("entity history chart with long name/unit failed wire.CheckView: %v", err)
	}
}

func TestHardeningTapNumAndWatchRuleRejectNaNInf(t *testing.T) {
	f := newFakeHA(t)
	f.mu.Lock()
	f.state["number.limit"] = State{
		EntityID:   "number.limit",
		State:      "10",
		Attributes: map[string]any{"friendly_name": "阈值", "min": 0.0, "max": 100.0, "step": 1.0},
	}
	f.mu.Unlock()
	a := newApp(func(string, ...any) {})
	a.configure(Config{URL: f.srv.URL, Writable: []string{"number"}}, f.token)

	for _, bad := range []string{"NaN", "Inf", "-Inf"} {
		if _, err := a.tap(context.Background(), "number.limit", "num", "", map[string]string{"value": bad}); err == nil {
			t.Fatalf("expected tap num to reject %q", bad)
		}
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if calls != 0 {
		t.Fatalf("expected 0 HA calls for non-finite num values, got %d", calls)
	}

	// Watch rules must also reject NaN/Inf.
	for _, line := range []string{"sensor.x >NaN", "sensor.x <Inf", "sensor.x >-Inf"} {
		if _, err := parseRule(line); err == nil {
			t.Fatalf("expected parseRule(%q) to fail", line)
		}
	}
	nan := math.NaN()
	inf := math.Inf(1)
	if err := (&Rule{Entity: "sensor.x", Above: &nan}).check(); err == nil {
		t.Fatalf("expected Rule.check to reject NaN Above")
	}
	if err := (&Rule{Entity: "sensor.x", Below: &inf}).check(); err == nil {
		t.Fatalf("expected Rule.check to reject Inf Below")
	}
}

func TestHardeningHomePageLargeHomePassesCheckView(t *testing.T) {
	f := newFakeHA(t)
	f.mu.Lock()
	f.state = map[string]State{}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var areas [][]any
	// 60 active rooms + 60 quiet rooms + 50 offline + 20 scenes + 15 alerts + 12 dials + 40 rooms with 2 devices?
	// Make 55 active rooms with 1 light each (total 55 <= 60 so byRoom=true) + 55 quiet rooms with 1 lock each,
	// plus 55 loose devices? No, quickList counts overviewDomains across all devices; put 55 lights in rooms
	// AND 55 loose locks/automations/offline? Wait: give each of 55 rooms a second overview-eligible device?
	// Even simpler: 30 active rooms with 2 lights (60 total -> byRoom=true) + 60 lively areas (each with temp sensor!)
	// + 60 quiet areas (each with 1 lock) -> roomGrids (60) + areaTiles lively (60) + areaTiles quiet (60) +
	// scenes (20) + alerts (12) + offline (51) + dials (12) + 50 more roomGrids via loose lights? Wait:
	// roomGrids puts 1 grid per room; each room has 1 light (55 rooms -> 55 tiles), PLUS 60 lively rooms (with temp
	// sensors -> 60 area tiles) + 60 quiet rooms (with locks -> 60 quiet area tiles) + 50 offline + 12 alerts +
	// 20 scenes + 12 dials + 40 remote tiles (via registry) or just 60 active rooms (60 tiles) + 60 quiet rooms (60 tiles)
	// + 60 area tiles (lively) + 50 offline + 12 alerts + 20 scenes + 12 dials = 274; plus add 30 rooms with temp+1 lock
	// Wait: with 60 active rooms (1 light each = 60 tiles) + 60 quiet rooms (1 lock each = 60 tiles) + 60 lively area tiles = 180 + 51 + 12 + 20 + 12 = 275.
	// What if UIVersion is 5 (row list: deviceList 60 + alertSection 63 + areaList 60)? Wait at UIVersion 6, roomGrids ALSO adds remotes!
	// Wait: let's add a registry with 45 IR remotes (2 buttons each) in rooms! Remotes don't count toward quickList total <= 60,
	// so roomGrids draws 60 lights + 45 remotes = 105 tiles -> 105 + 60 + 60 + 51 + 12 + 20 + 12 = 320 > 300!
	reg := &Registry{Entities: map[string]RegEntity{}, Devices: map[string]RegDevice{}, ByDevice: map[string][]string{}}
	for i := range 55 {
		aid := fmt.Sprintf("room_%02d", i)
		aname := fmt.Sprintf("房间 %02d", i)
		light := fmt.Sprintf("light.r%02d", i)
		offSw := fmt.Sprintf("switch.off_%02d", i)
		onBtn := fmt.Sprintf("button.ir_%02d_turn_on", i)
		offBtn := fmt.Sprintf("button.ir_%02d_turn_off", i)
		devID := fmt.Sprintf("dev_ir_%02d", i)
		f.state[light] = State{EntityID: light, State: "on", Attributes: map[string]any{"friendly_name": aname + "灯", "brightness": 128.0}, LastChanged: at}
		f.state[offSw] = State{EntityID: offSw, State: "unavailable", Attributes: map[string]any{"friendly_name": aname + "离线开关"}, LastChanged: at}
		f.state[onBtn] = State{EntityID: onBtn, State: "unknown", Attributes: map[string]any{"friendly_name": aname + "电视 开机"}, LastChanged: at}
		f.state[offBtn] = State{EntityID: offBtn, State: "unknown", Attributes: map[string]any{"friendly_name": aname + "电视 关机"}, LastChanged: at}
		reg.Devices[devID] = RegDevice{ID: devID, Name: aname + "电视"}
		reg.Entities[onBtn] = RegEntity{EntityID: onBtn, DeviceID: devID}
		reg.Entities[offBtn] = RegEntity{EntityID: offBtn, DeviceID: devID}
		reg.ByDevice[devID] = []string{onBtn, offBtn}
		areas = append(areas, []any{aid, aname, []string{light, offSw, onBtn, offBtn}})
	}
	for i := range 55 {
		aid := fmt.Sprintf("quiet_%02d", i)
		aname := fmt.Sprintf("储物 %02d", i)
		lk := fmt.Sprintf("lock.q_%02d", i)
		f.state[lk] = State{EntityID: lk, State: "locked", Attributes: map[string]any{"friendly_name": aname + "锁"}, LastChanged: at}
		areas = append(areas, []any{aid, aname, []string{lk}})
	}
	for i := range 20 {
		sc := fmt.Sprintf("scene.s_%02d", i)
		f.state[sc] = State{EntityID: sc, State: "2026-01-02T00:00:00Z", Attributes: map[string]any{"friendly_name": fmt.Sprintf("场景 %02d", i)}, LastChanged: at}
	}
	for i := range 15 {
		al := fmt.Sprintf("binary_sensor.leak_%02d", i)
		f.state[al] = State{EntityID: al, State: "on", Attributes: map[string]any{"friendly_name": fmt.Sprintf("漏水 %02d", i), "device_class": "moisture"}, LastChanged: at}
	}
	rawAreas, _ := json.Marshal(areas)
	f.areas = string(rawAreas)
	f.mu.Unlock()

	a := newApp(func(string, ...any) {})
	a.configure(Config{URL: f.srv.URL}, f.token)
	h, _, _ := a.client()
	h.mu.Lock()
	h.reg, h.regAt = reg, time.Now()
	h.mu.Unlock()

	// Unfold both offline devices (Lost: true) and quiet rooms (Rooms: true) on UI v6.
	v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Lost: true, Rooms: true}),
	}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("large home overview (Lost=true, Rooms=true) failed wire.CheckView: %v", err)
	}
}

func TestHardeningPinsMapBounded(t *testing.T) {
	pinMu.Lock()
	clear(pins)
	pinMu.Unlock()
	for i := range 1000 {
		pin(callAction, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)), "light.a")
	}
	pinMu.Lock()
	n := len(pins)
	clear(pins)
	pinMu.Unlock()
	if n > 512 {
		t.Fatalf("pins map grew unbounded: len=%d", n)
	}
}

func TestHardeningHTTPResponseOversizedRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 64<<10)
		for i := range chunk {
			chunk[i] = 'a'
		}
		for written := 0; written <= (32<<20)+128; written += len(chunk) {
			_, _ = w.Write(chunk)
		}
	}))
	defer srv.Close()

	h := newHA(srv.URL, "good-token", false)
	_, _, err := h.sendTyped(context.Background(), http.MethodGet, "/api/big", nil)
	if err == nil {
		t.Fatalf("expected sendTyped to reject oversized (>32 MiB) HTTP response instead of silently truncating")
	}
}
