package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// sliceStateBefore returns a shallow-cloned, time-clean view of st containing only entries first seen before cutoff
// (YYYY-MM-DD) and evidence lines strictly before cutoff, with Count and Last recomputed so neither entry.Score,
// entry.Hard nor L1 synthesis can peek at post-cutoff data (unified design §3.2).
func sliceStateBefore(st *store.State, cutoff string) *store.State {
	if st == nil || cutoff == "" {
		return st
	}
	out := &store.State{
		Version:    st.Version,
		Onboarding: st.Onboarding,
		Contacts:   st.Contacts,
		Graph:      sliceGraphBefore(st.Graph, cutoff),
		Entries:    make(map[string]*entry.Entry, len(st.Entries)),
	}
	for id, e := range st.Entries {
		if e == nil || (e.First != "" && e.First >= cutoff) {
			continue
		}
		cp := *e
		if len(e.Evidence) > 0 {
			var evs []entry.Evidence
			last := ""
			kinds := map[string]bool{}
			for _, ev := range e.Evidence {
				if ev.Day != "" && ev.Day >= cutoff {
					continue
				}
				evs = append(evs, ev)
				if ev.Day > last {
					last = ev.Day
				}
				if ev.Source != "" {
					kinds[ev.Source] = true
				}
			}
			cp.Evidence = evs
			if last != "" {
				cp.Last = last
			} else if cp.Last >= cutoff {
				cp.Last = cp.First
			}
			if len(evs) > 0 {
				stripped := len(evs) < len(e.Evidence)
				cp.Count = min(e.Count, len(evs))
				if cp.Count <= 0 {
					cp.Count = len(evs)
				}
				isOwned := entry.IsTrueUserOwned(&cp)
				if stripped && cp.Count <= 1 && cp.Status == entry.Stable && !isOwned {
					cp.Status = entry.Candidate
				}
				if len(kinds) > 0 {
					ks := make([]string, 0, len(kinds))
					for k := range kinds {
						ks = append(ks, k)
					}
					sort.Strings(ks)
					cp.Kinds = ks
				}
			}
		} else if cp.Last >= cutoff {
			cp.Last = cp.First
		}
		if len(e.Members) > 0 {
			cp.Members = slices.Clone(e.Members)
		}
		out.Entries[id] = &cp
	}
	return out
}

// sliceGraphBefore returns a time-clean view of g keeping only nodes created strictly before cutoff and assertions
// with ValidFrom strictly before cutoff, so 1-hop entity skeleton expansion cannot leak post-cutoff relations.
func sliceGraphBefore(g *store.Graph, cutoff string) *store.Graph {
	if g == nil || cutoff == "" {
		return g
	}
	out := &store.Graph{
		Nodes:      make(map[string]*store.Node, len(g.Nodes)),
		Assertions: make(map[string]*store.Assertion, len(g.Assertions)),
		NextNode:   g.NextNode,
	}
	for id, n := range g.Nodes {
		if n == nil || (n.Created != "" && n.Created >= cutoff) {
			continue
		}
		out.Nodes[id] = n
	}
	for id, a := range g.Assertions {
		if a == nil || (a.ValidFrom != "" && a.ValidFrom >= cutoff) {
			continue
		}
		out.Assertions[id] = a
	}
	return out
}

// collabOverviews reads a directory of time-clean L1 checkpoint snapshots named <YYYY-MM-DD>.json.
func collabOverviews(dir string) (map[string]*overview.State, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string]*overview.State{}
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".json")
		if len(base) != 10 {
			continue
		}
		if _, err := time.Parse("2006-01-02", base); err != nil {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		ov := &overview.State{}
		if err := json.Unmarshal(raw, ov); err != nil {
			return nil, fmt.Errorf("overview checkpoint %s: %w", f, err)
		}
		out[base] = ov
	}
	return out, nil
}

// collabOverviewFor returns the latest L1 checkpoint whose cutoff day is <= day.
// Because a checkpoint named YYYY-MM-DD was rebuilt with cutoff=YYYY-MM-DD (strictly < YYYY-MM-DD), snapDay <= day
// guarantees zero evidence from day or later.
func collabOverviewFor(ovs map[string]*overview.State, day string) *overview.State {
	best := ""
	for d := range ovs {
		if d <= day && d > best {
			best = d
		}
	}
	return ovs[best]
}

// hardevalL1Rebuild runs time-clean offline L1 rebuilds from scratch at one or more cutoff dates (YYYY-MM-DD),
// writing <out-dir>/<cutoff>.json for use with hardeval collab-run -overview-dir (unified design §3.2).
func hardevalL1Rebuild(args []string) error {
	fs := flag.NewFlagSet("hardeval l1-rebuild", flag.ContinueOnError)
	statePath := fs.String("state", "", "a copy of state.json (read only)")
	cutoffsSpec := fs.String("cutoffs", "", "comma-separated cutoff dates YYYY-MM-DD (each rebuild uses only data strictly before cutoff)")
	outDir := fs.String("out-dir", "", "output directory for <YYYY-MM-DD>.json checkpoints")
	cacheDir := fs.String("cache-dir", "", "model cache directory (default: <out-dir>/cache)")
	cmd := fs.String("model-cmd", "claude", "the model command line")
	longMdl := fs.String("long", "claude-sonnet-5-5", "medium-tier synthesis/contracts model")
	shortMdl := fs.String("short", "claude-haiku-4-5", "small-tier counter-example model")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *statePath == "" || *cutoffsSpec == "" || *outDir == "" {
		return errors.New("-state, -cutoffs and -out-dir are required")
	}
	var cutoffs []string
	for _, c := range strings.Split(*cutoffsSpec, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", c); err != nil {
			return fmt.Errorf("invalid cutoff %q (want YYYY-MM-DD)", c)
		}
		cutoffs = append(cutoffs, c)
	}
	if len(cutoffs) == 0 {
		return errors.New("empty -cutoffs")
	}
	st, err := readState(*statePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		return err
	}
	cDir := *cacheDir
	if cDir == "" {
		cDir = filepath.Join(*outDir, "cache")
	}
	m := model.CLI{
		Command: *cmd,
		Models: map[model.Tier]string{
			model.Small:  *shortMdl,
			model.Medium: *longMdl,
		},
		CacheDir: cDir,
		Limit:    model.Shared(),
	}
	for _, cutoff := range cutoffs {
		tCutoff, _ := time.Parse("2006-01-02", cutoff)
		refDay := tCutoff.AddDate(0, 0, -1)
		y, w := refDay.ISOWeek()
		week := fmt.Sprintf("%04d-W%02d", y, w)
		sliced := sliceStateBefore(st, cutoff)
		ovState := &overview.State{Patterns: map[string]*overview.Pattern{}}
		runner := &overview.Runner{
			M:     m,
			Long:  *longMdl,
			Short: *shortMdl,
			Lang:  "zh-CN",
			Now:   func() time.Time { return tCutoff },
		}
		start := time.Now()
		run, err := runner.Weekly(context.Background(), sliced, ovState, week, cutoff)
		if err != nil {
			return fmt.Errorf("cutoff %s (%s): %w", cutoff, week, err)
		}
		outPath := filepath.Join(*outDir, cutoff+".json")
		raw, err := json.MarshalIndent(ovState, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(outPath, raw, 0o600); err != nil {
			return err
		}
		pTot, pUsed, cTot, cUsed, cHard, cSens := 0, 0, 0, 0, 0, 0
		for _, p := range ovState.Patterns {
			if p.IsContract() {
				cTot++
				if p.Used() {
					cUsed++
					if len(p.Categories) > 0 || p.OriginSpace != "" {
						cSens++
					}
				}
				if p.Hard {
					cHard++
				}
			} else {
				pTot++
				if p.Used() {
					pUsed++
				}
			}
		}
		fmt.Printf("l1-rebuild cutoff=%s week=%s entries=%d took=%s calls=%d principles=%d/%d contracts=%d/%d(hard=%d,sens=%d) -> %s\n",
			cutoff, week, len(sliced.Entries), time.Since(start).Round(time.Second), run.Calls, pUsed, pTot, cUsed, cTot, cHard, cSens, outPath)
	}
	return nil
}
