package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// Every page the hub draws holds to wire.CheckView, whatever the computers answer: none switched on, one that cannot
// be reached, one with pairs and one without, very long text, and as many computers and pairs as there can be.
func TestPageCheckView(t *testing.T) {
	long := strings.Repeat("很长的一段话 with words ", 400)
	score, unnatural := 61, false
	full := func(n int) status {
		st := status{Version: "0.1.0", Mode: "block", Total: n, Due: n / 2,
			Dirs: []dirState{{Dir: "~/.claude", Settings: "/home/me/.claude/settings.json", Installed: true}}}
		for i := range n {
			st.Recent = append(st.Recent, entry{At: "2026-11-05T10:00:00Z", Original: fmt.Sprint("帮我看看 ", i), English: "look at it",
				Mode: "rewrite", Score: &score, Natural: &unnatural, NaturalHint: "Have a look at it, please."})
		}
		return st
	}
	longStatus := status{Mode: "warn", Total: 1,
		Dirs: []dirState{
			{Dir: long, Settings: long},
			{Dir: long, Error: long},
			{Dir: long, Installed: true},
		},
		Recent: []entry{{At: "not a time " + long, Original: long, English: long, Mode: long, Score: &score, Natural: &unnatural, NaturalHint: long}}}
	manyNames := make([]string, 40)
	manyAnswers := map[string]any{}
	for i := range manyNames {
		manyNames[i] = fmt.Sprint("m", i)
		manyAnswers[manyNames[i]] = full(recentShown)
	}
	cases := []struct {
		name     string
		machines []string
		answers  map[string]any
		call     nodeCaller
	}{
		{name: "no computer"},
		{name: "unreachable", machines: []string{"m1"}, answers: map[string]any{}},
		{name: "unreachable with a long error", machines: []string{"m1"}, call: func(context.Context, string, any) error {
			return errors.New(long)
		}},
		{name: "with pairs", machines: []string{"m1"}, answers: map[string]any{"m1": full(3)}},
		{name: "no pairs", machines: []string{"m1"}, answers: map[string]any{"m1": status{Mode: "log", Dirs: []dirState{{Dir: "~/.claude", Settings: "/home/me/.claude/settings.json"}}}}},
		{name: "no directories", machines: []string{"m1"}, answers: map[string]any{"m1": status{Mode: "block"}}},
		{name: "long text", machines: []string{long}, answers: map[string]any{long: longStatus}},
		{name: "many computers, full", machines: manyNames, answers: manyAnswers},
		{name: "mixed", machines: []string{"m1", "m2", "m3"}, answers: map[string]any{"m1": full(recentShown), "m3": status{Mode: "block"}}},
	}
	for _, c := range cases {
		for _, locale := range []string{"zh-Hans", "en", "ja"} {
			call := c.call
			if call == nil {
				call = fakeNodes(c.answers)
			}
			app := &hubApp{machines: c.machines, call: call}
			r := &ext.UIRequest{}
			r.Contribution, r.Size, r.UIVersion, r.Locale, r.TZ = logUI, wire.SizeFull, 3, locale, "Asia/Shanghai"
			v, err := app.render(context.Background(), r)
			if err != nil {
				t.Fatalf("%s/%s: %v", c.name, locale, err)
			}
			if err := wire.CheckView(v); err != nil {
				t.Errorf("%s/%s: %v", c.name, locale, err)
			}
		}
	}
}

// The pairs a page lists are shared out so the page keeps to wire.ItemsMost, and each section's footer counts the
// pairs it lists.
func TestPairsShown(t *testing.T) {
	full := status{Dirs: []dirState{{Dir: "~/.claude"}}, Recent: make([]entry, recentShown)}
	if n := pairsShown([]status{full}, []error{nil}); n != recentShown {
		t.Fatalf("one computer lists all its pairs, got %d", n)
	}
	states, errs := make([]status, 40), make([]error, 40)
	for i := range states {
		states[i] = full
	}
	n := pairsShown(states, errs)
	if n >= recentShown || 1+40*(1+2)+40*n > wire.ItemsMost {
		t.Fatalf("40 computers: %d pairs each", n)
	}
	sec := machineSection(texts("en"), time.UTC, "m1", full, nil, n)
	if want := fmt.Sprintf(texts("en").recent, n); sec.Footer != want {
		t.Fatalf("footer %q, want %q", sec.Footer, want)
	}
}
