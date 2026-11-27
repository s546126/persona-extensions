package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/curate"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona/sdk/go/ext"
)

// memory.entries answers from the published snapshot: not_ready before one, another contribution refused, a page
// after; enkeep_rerun_status reads it back as the hub would.
func TestMemoryEntries(t *testing.T) {
	a, st := toolApp(t)
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	_, err := a.memory(t.Context(), "entries", raw(map[string]any{"contribution": "memory", "limit": 200}))
	var coded *ext.Error
	if !errors.As(err, &coded) || coded.Code != "not_ready" {
		t.Fatalf("before a snapshot: %v", err)
	}
	st.S.Entries = map[string]*entry.Entry{"i-a": {ID: "i-a", Facet: entry.Interaction, Grain: entry.Class, Text: "report the conclusion first",
		Status: entry.Stable, Count: 2, Origin: entry.Behavior,
		Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting}}}
	a.c.Live = &curate.Live{}
	a.c.Publish()
	if _, err := a.memory(t.Context(), "entries", raw(map[string]any{"contribution": "other", "limit": 200})); err == nil {
		t.Error("another contribution answered")
	}
	if _, err := a.memory(t.Context(), "entries", raw(map[string]any{"contribution": "memory", "limit": 0})); err == nil {
		t.Error("limit 0 answered")
	}
	out, err := a.memory(t.Context(), "entries", raw(map[string]any{"contribution": "memory", "limit": 200}))
	r, _ := out.(memsrc.Result)
	// 1 curated entry (i-a, the standing set) + 2 profile summary entries (recalled on their own, after it).
	if err != nil || len(r.Entries) != 3 || r.Entries[0].ID != "i-a" || r.Entries[1].ID != "profile:overview" || r.Entries[2].ID != "profile:interaction" ||
		r.Entries[0].Status != "active" || !r.Entries[0].Standing || r.Entries[1].Standing || !r.Tagged {
		t.Fatalf("page: %+v %v", out, err)
	}
	status, _ := call(t, a, "enkeep_rerun_status", map[string]any{})
	if !strings.Contains(status, "memory.entries: 3 entr(ies) in 1 page(s) of 3 in the pool (3 active, 1 standing), tagged true (1 of the pool read before the 0.2.0-beta.40 tagging rules") {
		t.Fatalf("status: %q", status)
	}
}
