package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func boolPtr(b bool) *bool { return &b }

func TestSessionsPageLargeFleetStaysWithinCheckViewBudget(t *testing.T) {
	noSandbox(t)
	a := newTestApp(t)
	now := a.now()

	if err := a.db().update(func(st *State, now time.Time) error {
		st.ensureDefaults(now)
		local := st.machine("local")
		local.Platform = "darwin/arm64"
		local.BinaryVersion = "0.3.0"
		entries := make([]ColdEntry, 0, 60)
		for i := range 60 {
			entries = append(entries, ColdEntry{
				ID:         fmt.Sprintf("sess-%03d", i),
				SessionID:  fmt.Sprintf("sid-%03d", i),
				Tool:       "claude",
				Title:      fmt.Sprintf("Archived conversation %03d", i),
				Project:    "demo-repo",
				State:      "archived",
				Local:      true,
				Path:       fmt.Sprintf("/Users/dev/.claude/projects/demo/s-%03d.jsonl", i),
				ArchivedAt: now.Add(-time.Duration(i) * time.Hour).UTC().Format(time.RFC3339),
			})
		}
		st.setCold("local", &ColdCatalog{
			MachineID:   "local",
			Platform:    "darwin/arm64",
			CheckedAt:   now,
			RefreshedAt: now,
			CLI:         ColdCLI{Version: "0.3.0", Path: "/opt/homebrew/bin/disklessly"},
			Backend:     ColdBackend{ID: "backup", Type: "rclone", Reachable: boolPtr(true), CheckedAt: now, LatencyMs: 12},
			Total:       60,
			Entries:     entries,
		})
		for i := 1; i < maxMachines; i++ {
			id := fmt.Sprintf("node-%02d", i)
			m, err := st.upsertMachine(MachineInput{ID: id, Host: id, Name: id, Source: "manual"}, now)
			if err != nil {
				return err
			}
			m.Platform = "darwin/arm64"
			switch i % 4 {
			case 0:
				st.setCold(id, &ColdCatalog{
					MachineID:   id,
					Platform:    "darwin/arm64",
					CheckedAt:   now,
					RefreshedAt: now,
					Brew:        "/opt/homebrew/bin/brew",
					BrewKnown:   true,
					CLI:         ColdCLI{Version: "0.1.0", Path: "/usr/local/bin/disklessly"},
					State:       ColdCLIOld,
					Detail:      "unknown subcommand: sessions",
				})
			case 1:
				st.setCold(id, &ColdCatalog{
					MachineID:   id,
					Platform:    "darwin/arm64",
					CheckedAt:   now,
					RefreshedAt: now,
					Brew:        "/opt/homebrew/bin/brew",
					BrewKnown:   true,
					State:       ColdNoCLI,
				})
			case 2:
				st.setCold(id, &ColdCatalog{
					MachineID:   id,
					Platform:    "darwin/arm64",
					CheckedAt:   now,
					RefreshedAt: now,
					CLI:         ColdCLI{Version: "0.3.0"},
					Backend:     ColdBackend{ID: "nas", Type: "rclone", Reachable: boolPtr(false), Error: "connection refused", LatencyMs: 500},
					State:       ColdBackendDown,
				})
			default:
				st.setCold(id, &ColdCatalog{
					MachineID:   id,
					Platform:    "darwin/arm64",
					CheckedAt:   now,
					RefreshedAt: now,
					State:       ColdUnreadable,
					Error:       "invalid JSON output",
					Detail:      "unexpected EOF",
				})
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	deep := &deepResult{
		errs: []string{"ssh timeout node-01", "ssh timeout node-02", "ssh timeout node-03", "ssh timeout node-04"},
	}
	if err := a.db().read(func(st *State) {
		deep.hits = SearchCold(st, "", "", coldShown, now)
	}); err != nil {
		t.Fatal(err)
	}

	for _, uiVer := range []int{1, 2, 3, 4, 6, 8} {
		v, err := a.sessionsView(&ext.UIRequest{UIParams: wire.UIParams{UIVersion: uiVer}}, sessPage{Query: "demo"}, deep)
		if err != nil {
			t.Fatalf("sessionsView UIVersion %d: %v", uiVer, err)
		}
		checkStrictView(t, v, false)
	}

	// Verify all 64 machines remain visible and selecting a compact machine row opens its full view.
	v, err := a.renderSessions(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, v, false)
	var machSection *wire.Section
	for i := range v.Sections {
		if v.Sections[i].Key == "cold-machines" {
			machSection = &v.Sections[i]
			break
		}
	}
	if machSection == nil {
		t.Fatal("missing cold-machines section")
	}
	rowCount := 0
	var compactTarget, compactActionID string
	for _, it := range machSection.Items {
		if it.Type == wire.ComponentRow && strings.HasPrefix(it.Key, "mach-") {
			rowCount++
			if it.Action != nil && (it.Action.ID == "machine" || strings.HasPrefix(it.Action.ID, "machine:")) && compactTarget == "" {
				compactTarget = it.Action.Values["machine"]
				compactActionID = it.Action.ID
			}
		}
	}
	if rowCount != maxMachines {
		t.Fatalf("cold-machines row count = %d, want %d", rowCount, maxMachines)
	}
	if compactTarget == "" {
		t.Fatal("expected at least one compact tappable machine row on 64-machine fleet")
	}
	// UIv8 selects via Values, UIv1 selects via Action.ID without Values; both expand the single machine's controls.
	for _, tc := range []struct {
		ver    int
		action string
		values map[string]string
	}{
		{8, compactActionID, map[string]string{"machine": compactTarget}},
		{1, compactActionID, nil},
	} {
		res, err := a.actionSessions(ctx, &ext.UIRequest{UIParams: wire.UIParams{
			UIVersion: tc.ver,
			State:     v.State,
			Action:    tc.action,
			Values:    tc.values,
		}})
		if err != nil {
			t.Fatalf("UIVersion %d selecting compact machine %s: %v", tc.ver, compactTarget, err)
		}
		checkStrictView(t, res.View, false)
		hasMachineAction := false
		for _, sec := range res.View.Sections {
			for _, it := range sec.Items {
				if it.Type == wire.ComponentButton && it.Action != nil && it.Action.Values["m"] == compactTarget {
					hasMachineAction = true
				}
			}
		}
		if !hasMachineAction {
			t.Fatalf("UIVersion %d: expected expanded machine actions for %s", tc.ver, compactTarget)
		}
	}

	// Verify sandbox banner + open details + running job all remain within CheckView budget.
	prevSandbox := sandboxed
	sandboxed = func() bool { return true }
	t.Cleanup(func() { sandboxed = prevSandbox })
	a.jobs.mu.Lock()
	if a.jobs.jobs == nil {
		a.jobs.jobs = map[string]*coldJob{}
	}
	a.jobs.jobs["node-01"] = &coldJob{Kind: jobRefresh, Label: "正在刷新", Started: now}
	a.jobs.mu.Unlock()
	for _, uiVer := range []int{1, 4, 8} {
		sv, err := a.sessionsView(&ext.UIRequest{UIParams: wire.UIParams{UIVersion: uiVer}}, sessPage{Query: "demo", Open: detailsKey("cold", "node-03")}, deep)
		if err != nil {
			t.Fatalf("sandboxed sessionsView UIVersion %d: %v", uiVer, err)
		}
		checkStrictView(t, sv, false)
	}
}

func TestFleetTabMediumAndLargeErroredFleetStaysWithinCheckViewBudget(t *testing.T) {
	noSandbox(t)
	for _, count := range []int{28, maxMachines} {
		t.Run(fmt.Sprintf("%d-machines", count), func(t *testing.T) {
			a := newTestApp(t)
			now := a.now()
			if err := a.db().update(func(st *State, now time.Time) error {
				st.ensureDefaults(now)
				local := st.machine("local")
				local.LastError = "SSH 连接 local 失败：Permission denied (publickey)"
				st.Scans["local"] = ParseProbeOutput(local, sampleProbeLocal, now)
				for i := 1; i < count; i++ {
					id := fmt.Sprintf("err-node-%02d", i)
					m, err := st.upsertMachine(MachineInput{ID: id, Host: id, Name: id, Source: "manual"}, now)
					if err != nil {
						return err
					}
					m.LastError = fmt.Sprintf("SSH 连接 %s 失败：ssh: connect to host %s port 22: Connection timed out", id, id)
					st.Scans[id] = ParseProbeOutput(m, sampleProbeLocal, now)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			// Also attach completed jobs with raw logs so machineJobItems reaches maximum component count.
			a.jobs.mu.Lock()
			if a.jobs.jobs == nil {
				a.jobs.jobs = map[string]*coldJob{}
			}
			for i := 1; i < count; i++ {
				id := fmt.Sprintf("err-node-%02d", i)
				a.jobs.jobs[id] = &coldJob{
					Kind:     jobClean,
					Label:    "正在执行一键安全清理",
					Started:  now.Add(-2 * time.Minute),
					Finished: now.Add(-time.Minute),
					Err:      "ssh timeout",
					Raw:      "stderr: connection timed out",
				}
			}
			a.jobs.mu.Unlock()

			for _, uiVer := range []int{1, 2, 3, 4, 6, 8} {
				v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: uiVer}})
				if err != nil {
					t.Fatalf("count=%d UIVersion=%d: %v", count, uiVer, err)
				}
				checkStrictView(t, v, false)
			}
		})
	}
}

func TestItemsTabLargeScanStaysWithinCheckViewBudget(t *testing.T) {
	noSandbox(t)
	a := newTestApp(t)
	if err := a.db().update(func(st *State, now time.Time) error {
		st.ensureDefaults(now)
		local := st.machine("local")
		sc := ParseProbeOutput(local, sampleProbeLocal, now)
		sc.Items = nil
		for i := range 320 {
			id := fmt.Sprintf("cache-item-%03d", i)
			if i == 0 {
				id = "very-long-item-id-" + strings.Repeat("a", 130)
			}
			sc.Items = append(sc.Items, ScanItem{
				ID:       id,
				Category: CatGoCache,
				Title:    fmt.Sprintf("Synthetic cache artifact %03d", i),
				Path:     fmt.Sprintf("/Users/dev/.cache/item-%03d", i),
				Bytes:    uint64(320-i) * mib,
				Safety:   SafetySafe,
				Action:   "clean",
				Reason:   "rebuildable cache",
			})
		}
		st.Scans["local"] = sc
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, uiVer := range []int{1, 2, 3, 4, 6, 8} {
		v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
			UIVersion: uiVer,
			State:     ext.EncodeState(page{Machine: "local", Tab: tabItems}),
		}})
		if err != nil {
			t.Fatalf("itemsTab UIVersion=%d: %v", uiVer, err)
		}
		checkStrictView(t, v, false)
		foundNotice := false
		for _, sec := range v.Sections {
			if strings.Contains(sec.Footer, "320") {
				foundNotice = true
			}
		}
		if !foundNotice {
			t.Fatalf("UIVersion %d: expected truncation notice mentioning 320 total items", uiVer)
		}
	}
}

func TestLongIDsAndEmptyTitlesStayWithinCheckViewLimits(t *testing.T) {
	noSandbox(t)
	// Defensive clip bounds check (must not panic on <= 0).
	if got := clip("hello", 0); got != "" {
		t.Fatalf("clip(0) = %q, want empty", got)
	}
	if got := clip("hello", -3); got != "" {
		t.Fatalf("clip(-3) = %q, want empty", got)
	}
	if got := clip("hello", 1); utf8.RuneCountInString(got) > 1 {
		t.Fatalf("clip(1) = %q, want <= 1 rune", got)
	}

	a := newTestApp(t)
	longID := "session-" + strings.Repeat("x", 150)
	if err := a.db().update(func(st *State, now time.Time) error {
		st.ensureDefaults(now)
		st.setCold("local", &ColdCatalog{
			MachineID:   "local",
			Platform:    "darwin/arm64",
			CheckedAt:   now,
			RefreshedAt: now,
			CLI:         ColdCLI{Version: "0.3.0"},
			Backend:     ColdBackend{ID: "backup", Type: "rclone", Reachable: boolPtr(true), CheckedAt: now, LatencyMs: 5},
			Total:       2,
			Entries: []ColdEntry{
				{
					ID:         longID,
					Tool:       "claude",
					Title:      "",
					State:      "archived",
					Local:      true,
					ArchivedAt: now.UTC().Format(time.RFC3339),
				},
				{
					ID:         "",
					Tool:       "codex",
					Title:      "   ",
					State:      "archived",
					Local:      true,
					ArchivedAt: now.Add(-time.Minute).UTC().Format(time.RFC3339),
				},
			},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, uiVer := range []int{1, 4, 8} {
		v, err := a.renderSessions(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: uiVer}})
		if err != nil {
			t.Fatalf("renderSessions UIVersion=%d: %v", uiVer, err)
		}
		checkStrictView(t, v, false)
	}
}

func TestLongMachineIDsFilterCapsAndAdversarialDuplicatesSatisfyCheckView(t *testing.T) {
	noSandbox(t)
	a := newTestApp(t)
	longMachID := "m-" + strings.Repeat("a", 123) // 125 chars, valid <= 128 bytes
	if err := a.db().update(func(st *State, now time.Time) error {
		st.ensureDefaults(now)
		m, err := st.upsertMachine(MachineInput{ID: longMachID, Host: "host.example.com", Name: "Long ID Node", Source: "manual"}, now)
		if err != nil {
			return err
		}
		sc := ParseProbeOutput(m, sampleProbeLocal, now)
		// Adversarial duplicate ScanItem IDs that would collide under a single-step fallback.
		sc.Items = []ScanItem{
			{ID: "2-foo", Category: CatGoCache, Title: "First", Path: "/tmp/a", Bytes: 3 * mib, Safety: SafetySafe, Action: "clean", Reason: "cache"},
			{ID: "foo", Category: CatGoCache, Title: "Second", Path: "/tmp/b", Bytes: 2 * mib, Safety: SafetySafe, Action: "clean", Reason: "cache"},
			{ID: "foo", Category: CatGoCache, Title: "Third", Path: "/tmp/c", Bytes: mib, Safety: SafetySafe, Action: "clean", Reason: "cache"},
		}
		st.Scans[m.ID] = sc
		st.History = append(st.History,
			&CleanRecord{ID: "restore-" + longMachID + "-1700000000000", MachineID: longMachID, MachineName: m.Name, Host: m.Host, Summary: "ok", FreedBytes: mib, CreatedAt: now},
			&CleanRecord{ID: "restore-" + longMachID + "-1700000000000", MachineID: longMachID, MachineName: m.Name, Host: m.Host, Summary: "ok2", FreedBytes: mib, CreatedAt: now},
		)
		st.setCold(longMachID, &ColdCatalog{
			MachineID:   longMachID,
			Platform:    "darwin/arm64",
			CheckedAt:   now,
			RefreshedAt: now,
			CLI:         ColdCLI{Version: "0.3.0"},
			Backend:     ColdBackend{ID: "b", Type: "rclone", Reachable: boolPtr(true), CheckedAt: now, LatencyMs: 5},
			Total:       3,
			Entries: []ColdEntry{
				{ID: "x", Tool: "claude", Title: "A", State: "archived", Local: true, ArchivedAt: now.UTC().Format(time.RFC3339)},
				{ID: "x", Tool: "claude", Title: "B", State: "archived", Local: true, ArchivedAt: now.Add(-time.Minute).UTC().Format(time.RFC3339)},
				{ID: "x", Tool: "claude", Title: "C", State: "archived", Local: true, ArchivedAt: now.Add(-2 * time.Minute).UTC().Format(time.RFC3339)},
			},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for _, tab := range []string{tabFleet, tabItems, tabPlan, tabSessions} {
		for _, uiVer := range []int{1, 4, 8} {
			v, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{
				UIVersion: uiVer,
				State:     ext.EncodeState(page{Machine: longMachID, Tab: tab}),
			}})
			if err != nil {
				t.Fatalf("longMachID tab=%s uiVer=%d: %v", tab, uiVer, err)
			}
			checkStrictView(t, v, false)
		}
	}
	cardV, err := a.card(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, Size: wire.SizeCompact}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, cardV, true)
	sessV, err := a.renderSessions(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, State: ext.EncodeState(sessPage{Machine: longMachID})}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, sessV, false)

	// Hand-edited state with 110 enabled machines (> 100 Filter.Choices schema max) + sandbox enabled.
	prevSandbox := sandboxed
	sandboxed = func() bool { return true }
	t.Cleanup(func() { sandboxed = prevSandbox })
	a2 := newTestApp(t)
	if err := a2.db().update(func(st *State, now time.Time) error {
		st.ensureDefaults(now)
		for i := 1; i <= 110; i++ {
			id := fmt.Sprintf("hand-%03d", i)
			st.Machines = append(st.Machines, &Machine{ID: id, Host: id, Name: id, Source: "manual", Enabled: true})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, uiVer := range []int{1, 4, 8} {
		fv, err := a2.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: uiVer, State: ext.EncodeState(page{Machine: "hand-110", Tab: tabFleet})}})
		if err != nil {
			t.Fatalf("110-machine fleet uiVer=%d: %v", uiVer, err)
		}
		checkStrictView(t, fv, false)
		sv, err := a2.renderSessions(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: uiVer, State: ext.EncodeState(sessPage{Machine: "hand-110"})}})
		if err != nil {
			t.Fatalf("110-machine sessions uiVer=%d: %v", uiVer, err)
		}
		checkStrictView(t, sv, false)
	}

	// Hand-edited state with 0 enabled machines: sessionsPage must omit Filter instead of failing minItems: 2.
	sv0 := (draw{
		st:        &State{Machines: []*Machine{{ID: "disabled-1", Host: "h", Name: "Disabled", Enabled: false}}},
		now:       time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
		loc:       time.UTC,
		uiVersion: 8,
	}).sessionsPage(sessPage{}, nil)
	checkStrictView(t, sv0, false)
	if sv0.Filter != nil {
		t.Fatalf("expected nil Filter when 0 machines enabled, got %+v", sv0.Filter)
	}
}
