package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
)

// hardevalStanding prints the standing set memory.entries answers for a copy of the state and the overview, and why
// each overview pattern is in it or not (its score against the set's lowest, or the template filter): a diagnosis,
// read only.
func hardevalStanding(args []string) error {
	fs := flag.NewFlagSet("hardeval standing", flag.ContinueOnError)
	statePath := fs.String("state", "", "a copy of state.json (read only)")
	ovPath := fs.String("overview", "", "a copy of overview-state.json (read only)")
	room := fs.Int("room", 4096, "the standing room in bytes (resident_bytes)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *statePath == "" {
		return errors.New("-state is required")
	}
	st, err := readState(*statePath)
	if err != nil {
		return err
	}
	var extras []memsrc.Extra
	if *ovPath != "" {
		b, err := os.ReadFile(*ovPath)
		if err != nil {
			return err
		}
		ov := &overview.State{}
		if err := json.Unmarshal(b, ov); err != nil {
			return err
		}
		extras = overview.Exits(ov)
	}
	var standing []memsrc.Entry
	after := ""
	for {
		r, _, err := memsrc.PageWith(st, nil, extras, memsrc.Params{Contribution: "memory", Limit: memsrc.MaxLimit, After: after, ResidentBytes: *room})
		if err != nil {
			return err
		}
		for _, e := range r.Entries {
			if e.Standing {
				standing = append(standing, e)
			}
		}
		if !r.HasMore || len(r.Entries) == 0 || !r.Entries[len(r.Entries)-1].Standing {
			break
		}
		after = r.After
	}
	byID := map[string]memsrc.Extra{}
	for _, x := range extras {
		byID[x.Entry.ID] = x
	}
	in := map[string]bool{}
	low := 1000
	bytes := 0
	bucketCount := map[string]int{}
	bucketBytes := map[string]int{}
	polCount := map[string]int{}
	dimCount := map[string]int{}
	clippedCount := 0
	fmt.Printf("standing set, room %d bytes:\n", *room)
	for _, e := range standing {
		in[e.ID] = true
		sc := -1
		bucket := "unknown"
		tag := ""
		if x, ok := byID[e.ID]; ok {
			sc = x.Score
			bucket = x.L1Kind
			if bucket == memsrc.BucketContract {
				tag = "pol=" + x.Polarity
				if x.Hard {
					tag += ",hard"
				}
				polCount[x.Polarity]++
			} else {
				tag = "dim=" + x.Dimension
				dimCount[x.Dimension]++
			}
		} else if x := st.Entries[e.ID]; x != nil {
			sc = entry.Score(x)
			if e.Rank >= 990 {
				bucket = memsrc.BucketHot
				if x.Pinned {
					tag = "pinned"
				} else {
					tag = "fast_hot"
				}
			} else if entry.KindOf(x) == entry.KindRules {
				bucket = "cold_contract"
			} else {
				bucket = "cold_principle"
			}
		}
		if strings.Contains(e.Text, "…") {
			clippedCount++
		}
		low = min(low, sc)
		sz := len(e.Text) + 3
		bytes += sz
		bucketCount[bucket]++
		bucketBytes[bucket] += sz
		fmt.Printf("  %4d  score %3d  %-10s %-18s %3dB  %-22s %s\n", e.Rank, sc, bucket, tag, sz, e.ID, clipRunes(strings.ReplaceAll(e.Text, "\n", " "), 70))
	}
	fmt.Printf("  total: %d lines, %d/%d bytes (unused %d B), lowest score %d, clipped %d\n", len(standing), bytes, *room, *room-bytes, low, clippedCount)
	fmt.Printf("  buckets: hot=%d (%d B), profile=%d (%d B), principle=%d (%d B), contract=%d (%d B)\n",
		bucketCount[memsrc.BucketHot], bucketBytes[memsrc.BucketHot],
		bucketCount[memsrc.BucketProfile], bucketBytes[memsrc.BucketProfile],
		bucketCount[memsrc.BucketPrinciple], bucketBytes[memsrc.BucketPrinciple],
		bucketCount[memsrc.BucketContract], bucketBytes[memsrc.BucketContract])
	fmt.Printf("  contract polarities: %v | principle dimensions: %v\n\npatterns:\n", polCount, dimCount)
	tpl := entry.TemplateQuotes(st.Entries)
	sort.Slice(extras, func(i, j int) bool { return extras[i].Score > extras[j].Score })
	for _, x := range extras {
		why := "in"
		switch {
		case in[x.Entry.ID]:
		case x.Kind == "":
			why = "not standing by kind (a category or space pattern, or a facet with no side)"
		case entry.TemplateBacked(st.Entries, x.Evidence, tpl):
			why = "template-backed"
		case x.Score < low:
			why = "score below the set's lowest"
		default:
			why = "alike to a chosen line, or no room"
		}
		fmt.Printf("  score %3d  %-10s %-40s %s | %s\n", x.Score, x.Kind, x.Entry.ID, why, clipRunes(strings.ReplaceAll(x.Entry.Text, "\n", " "), 50))
	}
	return nil
}
