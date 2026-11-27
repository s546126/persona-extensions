package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// dumpFleet is mixedFleet with the two pages out of step the way a long-running hub gets: the
// AI 对话 page found studio-mac's network drive without a repository three days ago and a later
// 硬盘空间 check could not reach it; offline-box was last reached twelve days ago; nas-box's
// stored check has an older version's item names.
func dumpFleet(t *testing.T) *app {
	t.Helper()
	a := mixedFleet(t)
	if err := a.db().update(func(st *State, now time.Time) error {
		c := *st.cold("studio-mac")
		c.CheckedAt = now.Add(-72 * time.Hour)
		st.setCold("studio-mac", &c)
		m := st.machine("studio-mac")
		m.LastError, m.Reachable = unreachableErrors[0].err, false
		o := *st.cold("offline-box")
		o.CheckedAt = now.Add(-10 * 24 * time.Hour)
		st.setCold("offline-box", &o)
		st.Scans["offline-box"].ScannedAt = now.Add(-12 * 24 * time.Hour)
		// nas-box's check was stored by an older version, in its words.
		renameItem(st.Scans["nas-box"], "cache-playwright", oldPlaywrightTitle, oldPlaywrightReason)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	setJob(a, "studio-mac", jobScan, unreachableErrors[0].err)
	return a
}

// dumpView writes v as plain text in drawing order, one component a line with its type, so a
// change of order or wording shows in a diff.
func dumpView(v *wire.View) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("# %s", v.Title)
	if v.Tabs != nil {
		var tabs []string
		for _, t := range v.Tabs.Items {
			mark := ""
			if t.Value == v.Tabs.Value {
				mark = "*"
			}
			tabs = append(tabs, mark+t.Title)
		}
		line("[tabs] %s", strings.Join(tabs, " | "))
	}
	for _, s := range v.Sections {
		line("")
		line("== %s", s.Title)
		for _, c := range s.Items {
			parts := []string{}
			for _, p := range []string{c.Title, c.Label, c.Subtitle, c.Detail, c.Text, c.Caption} {
				if strings.TrimSpace(p) != "" {
					parts = append(parts, p)
				}
			}
			if c.Badge != nil {
				parts = append(parts, "〔"+c.Badge.Text+"〕")
			}
			if c.Type == wire.ComponentCode {
				parts = append(parts, c.Path+": "+strings.ReplaceAll(c.Text, "\n", " ⏎ "))
			}
			if c.Icon != "" {
				parts = append([]string{"<" + c.Icon + ">"}, parts...)
			}
			line("[%s] %s", c.Type, strings.Join(parts, " · "))
			for _, st := range c.Stats {
				line("    %s: %s (%s)", st.Label, st.Value, st.Detail)
			}
			for _, r := range c.Rows {
				line("    | %s", strings.Join(r, " | "))
			}
			if c.Action != nil && c.Action.Push != nil {
				line("    -> push %s", c.Action.Push.Title)
			}
		}
		if s.Footer != "" {
			line("-- %s", s.Footer)
		}
	}
	return b.String()
}

// TestDumpPages writes both pages of dumpFleet as text to $DISKLESSLY_DUMP_TEXT_DIR, for people
// to read before and after a wording change; it checks nothing else.
func TestDumpPages(t *testing.T) {
	dir := os.Getenv("DISKLESSLY_DUMP_TEXT_DIR")
	if dir == "" {
		t.Skip("DISKLESSLY_DUMP_TEXT_DIR not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := dumpFleet(t)
	views := map[string]*wire.View{
		"disk-fleet.txt":        diskView(t, a, "local", tabFleet),
		"disk-items-studio.txt": diskView(t, a, "studio-mac", tabItems),
		"sessions-all.txt":      sessView(t, a, ""),
		"sessions-studio.txt":   sessView(t, a, "studio-mac"),
		// studio-mac reachable, its drive without a repository, nothing moved from it yet.
		"sessions-studio-reachable.txt": sessView(t, mixedFleet(t), "studio-mac"),
	}
	if all := findComponentByKey(views["disk-fleet.txt"], "steps-all"); all != nil && all.Action.Push != nil {
		// The page that lists every step (0.5.3).
		res, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, State: all.Action.Push.State}})
		if err != nil {
			t.Fatal(err)
		}
		views["disk-advice-all.txt"] = res
	}
	for name, v := range views {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(dumpView(v)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
