package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHardeningSourcesTabStatsBoundedAtSix(t *testing.T) {
	// Cap cold scan at 3 files and write 5 cold indexes (1 unconfirmed + 4 archived)
	// so both UnconfirmedSessions > 0 AND ArchivedOmitted > 0.
	orig := maxScannedColdFiles
	maxScannedColdFiles = 3
	t.Cleanup(func() { maxScannedColdFiles = orig })

	coldDir := t.TempDir()
	// Write 4 archived files with older mtime first, then 1 unconfirmed file with newest mtime
	// so loadColdSessions (sorted by filesystem mtime) scans the unconfirmed file AND omits >= 1 archived file.
	baseTime := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("entry-arch-%d", i)
		writeColdIndex(t, coldDir, id, map[string]any{
			"sessionId":    fmt.Sprintf("s-arch-%d", i),
			"originalPath": fmt.Sprintf("/tmp/a%d.jsonl", i),
			"mtime":        fmt.Sprintf("2026-11-0%dT10:00:00Z", i),
		}, msg("user", "hello archived"))
		mt := baseTime.Add(time.Duration(i) * time.Hour)
		_ = os.Chtimes(filepath.Join(coldDir, id+".jsonl"), mt, mt)
	}
	writeColdIndex(t, coldDir, "entry-unconfirmed", map[string]any{
		"sessionId":    "s-unconfirmed",
		"originalPath": "/tmp/u0.jsonl",
		"mtime":        "2026-11-08T10:00:00Z",
		"archivedAt":   "",
		"segment":      "",
	}, msg("user", "hello unconfirmed"))
	newest := baseTime.Add(24 * time.Hour)
	_ = os.Chtimes(filepath.Join(coldDir, "entry-unconfirmed.jsonl"), newest, newest)

	a := newApp(t.TempDir())
	a.now = func() time.Time { return t0 }
	if err := a.db().update(func(st *State, _ time.Time) error {
		st.ColdTextDirs = []string{coldDir}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 6,
		State:     ext.EncodeState(page{Tab: "sources"}),
	}})
	if err != nil {
		t.Fatalf("render sources: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("sources tab with unconfirmed + omitted archived sessions failed wire.CheckView: %v", err)
	}
}

func TestHardeningOversizedStateAndLongCodePassCheckView(t *testing.T) {
	a := newApp(t.TempDir())
	a.now = func() time.Time { return t0 }

	// 1. Oversized Query / HitID / EditRecipeID in incoming state.
	huge := strings.Repeat("查", 3000)
	for _, st := range []page{
		{Tab: "results", Query: huge},
		{Tab: "code", Query: huge},
		{Mode: modeHit, HitID: huge},
		{Mode: modeRecipeEdit, Query: huge, EditRecipeID: huge},
	} {
		v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
			UIVersion: 6,
			State:     ext.EncodeState(st),
		}})
		if err != nil {
			t.Fatalf("render %+v: %v", st.Mode, err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("render with oversized state (%q/%q) failed wire.CheckView: %v", st.Tab, st.Mode, err)
		}
	}

	// 2. Valid pipeline code > 2000 runes (maxCodeLen is 4000 bytes).
	longQuery := strings.Repeat("a", 2100)
	longCode := fmt.Sprintf(`scan_sessions(query="%s", scope="conversation") |> limit(n=10)`, longQuery)
	if err := a.db().update(func(st *State, now time.Time) error {
		_, err := st.upsertRecipe(RecipeInput{
			ID:    "long-recipe",
			Name:  "长代码配方",
			Scope: "conversation",
			Code:  longCode,
		}, now)
		return err
	}); err != nil {
		t.Fatalf("upsert long recipe: %v", err)
	}

	// Run code action with > 2000-rune code.
	res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 6,
		Action:    "run-code",
		Values:    map[string]string{"custom_code": longCode},
	}})
	if err != nil {
		t.Fatalf("action run-code: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("run-code result view failed wire.CheckView: %v", err)
	}

	// Code tab with > 2000-rune LastRun.Program.CodeListing.
	vCode, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 6,
		State:     ext.EncodeState(page{Tab: "code"}),
	}})
	if err != nil {
		t.Fatalf("render code tab: %v", err)
	}
	if err := wire.CheckView(vCode); err != nil {
		t.Fatalf("code tab with >2000-rune pipeline failed wire.CheckView: %v", err)
	}

	// Recipe edit view for a recipe whose saved code exceeds 2000 runes must pass wire.CheckView
	// and must not place truncated code into a saveable text field.
	vEdit, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 6,
		State:     ext.EncodeState(page{Mode: modeRecipeEdit, EditRecipeID: "long-recipe"}),
	}})
	if err != nil {
		t.Fatalf("render recipe edit: %v", err)
	}
	if err := wire.CheckView(vEdit); err != nil {
		t.Fatalf("recipe edit view with >2000-rune code failed wire.CheckView: %v", err)
	}
	for _, sec := range vEdit.Sections {
		for _, it := range sec.Items {
			if it.Type == wire.ComponentField && it.Name == "code" && it.Value != longCode {
				t.Fatalf("recipe edit field silently truncated >2000-rune code (len=%d)", len(it.Value))
			}
		}
	}
}

func TestHardeningDenseDetailTablesAndSourcesPassCheckView(t *testing.T) {
	a := newApp(t.TempDir())
	a.now = func() time.Time { return t0 }

	// Seed a dense LastRun with 120 machines, empty hit title, >2000-rune snippets, 120 snippets, 120 correlated refs.
	longText := strings.Repeat("证据文本", 800)
	snips := make([]EvidenceSnippet, 120)
	for i := range snips {
		snips[i] = EvidenceSnippet{Role: "User", Line: i + 1, Text: longText}
	}
	corrs := make([]CorrelatedRef, 120)
	for i := range corrs {
		corrs[i] = CorrelatedRef{ID: fmt.Sprintf("session:c-%d", i), Title: "", Reason: longText}
	}
	machines := make([]MachineRun, 120)
	for i := range machines {
		machines[i] = MachineRun{ID: fmt.Sprintf("m-%d", i), Name: fmt.Sprintf("node-%d", i), Key: fmt.Sprintf("node-%d", i), Status: machineOnline, Note: longText}
	}
	sessionDirs := make([]string, 310)
	for i := range sessionDirs {
		sessionDirs[i] = fmt.Sprintf("/tmp/sac-dir-%03d", i)
	}
	if err := a.db().update(func(st *State, now time.Time) error {
		st.SessionDirs = sessionDirs
		st.LastRun = &ExecutionResult{
			Program: SearchProgram{
				Title:       "dense",
				Scope:       "conversation",
				CodeListing: `scan_sessions(query="", scope="conversation") |> limit(n=25)`,
			},
			Hits: []*SearchHit{{
				ID:         "session:dense-1",
				SourceKind: "session",
				SessionID:  "dense-1",
				Title:      "", // empty title should not crash wire.CheckView
				Project:    strings.Repeat("p", 2100),
				Harness:    "claude-code",
				UpdatedAt:  t0,
				Snippets:   snips,
				Correlated: corrs,
				Files:      []string{strings.Repeat("a/", 1100) + "x.go"},
			}},
			TotalMatches:    1,
			ScannedSessions: 1,
			Machines:        machines,
			RanAt:           t0,
		}
		st.Restores = []RestoreNotice{{EntryID: "", Title: "", Path: longText, Message: longText, At: t0}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, p := range []page{
		{Tab: "results"},
		{Tab: "code"},
		{Tab: "sources"},
		{Mode: modeHit, HitID: "session:dense-1"},
	} {
		v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
			UIVersion: 6,
			State:     ext.EncodeState(p),
		}})
		if err != nil {
			t.Fatalf("render %+v: %v", p, err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("dense view %+v failed wire.CheckView: %v", p, err)
		}
	}
}

func TestHardeningHubClientMachinesRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"machines":[`))
		item := `{"id":"m1","name":"` + strings.Repeat("a", 1000) + `","online":true}`
		for written := 0; written <= (4<<20)+128; {
			if written > 0 {
				_, _ = w.Write([]byte(","))
				written++
			}
			n, _ := w.Write([]byte(item))
			written += n
		}
		_, _ = w.Write([]byte(`]}`))
	}))
	defer srv.Close()

	hc := &hubClient{base: srv.URL, token: "tok", client: srv.Client()}
	_, err := hc.Machines(context.Background())
	if err == nil {
		t.Fatalf("expected hubClient.Machines to reject oversized (>4 MiB) response")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected explicit too large error, got: %v", err)
	}
}
