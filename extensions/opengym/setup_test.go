package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The manifest requires neither the key nor the address, so the hub starts the program before either is filled in
// and the program's own page, card, status and tools are what tell the user what to do.

// noNetwork fails every request and counts them.
type noNetwork struct {
	mu sync.Mutex
	n  int
}

func (r *noNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return nil, errors.New("no network in this test")
}

func (r *noNetwork) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// unconfigured is an app that may not reach any server, and the lines it logs.
func unconfigured(t *testing.T, server, token string) (*app, *noNetwork, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	a := newApp(func(f string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, args...))
		mu.Unlock()
	})
	rt := &noNetwork{}
	a.client = &http.Client{Transport: rt}
	a.configure(config{ServerURL: server, ExerciseNames: true}, token)
	return a, rt, &lines
}

func TestManifestRequiresNoSecretAndNoSetting(t *testing.T) {
	var m struct {
		Secrets       []map[string]any
		Configuration map[string]any
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Secrets {
		if s["required"] == true {
			t.Errorf("secret %v is required: the hub would not start the program, and its setup page would never show", s["name"])
		}
	}
	if r, ok := m.Configuration["required"]; ok {
		t.Errorf("configuration requires %v", r)
	}
}

func TestUIContributionTitlesDiffer(t *testing.T) {
	var m struct {
		Contributes struct {
			UI []struct{ ID, Title string }
		}
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, u := range m.Contributes.UI {
		if other, ok := seen[u.Title]; ok {
			t.Errorf("%s and %s are both titled %q", other, u.ID, u.Title)
		}
		seen[u.Title] = u.ID
		if u.ID == cardUI && u.Title != cardTitle {
			t.Errorf("card title %q in the manifest, %q on the card", u.Title, cardTitle)
		}
	}
	a, _, _ := unconfigured(t, "", "")
	v, _ := a.renderCard(context.Background(), uiRequest(cardUI, 6, "", "", nil))
	if v.Title != "健身 · 概况" {
		t.Errorf("card header %q", v.Title)
	}
}

// TestUnconfiguredStartDrawsTheSetupPageAndTheCard starts the program as the hub does with nothing filled in, and
// draws the page and the card at every UI version.
func TestUnconfiguredStartDrawsTheSetupPageAndTheCard(t *testing.T) {
	for _, c := range []struct {
		name, config string
		secrets      map[string]string
		badges       string
		card         []string
	}{
		{"nothing", `{}`, nil, "还没填,还没填", []string{serverTitle, tokenTitle}},
		{"address only", `{"server_url":"https://gym.example.com"}`, nil, "已填,还没填", []string{tokenTitle}},
		{"key only", `{}`, map[string]string{secretToken: fakeToken}, "还没填,已填", []string{serverTitle}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := exttest.StartWith(t, manifest, exttest.Serve(t, program(newApp(t.Logf))), func(p *wire.InitializeParams) {
				p.Config = json.RawMessage(c.config)
				p.Secrets = c.secrets
			})
			for v := 1; v <= 6; v++ {
				var page wire.View
				if err := h.Call("ui.render", wire.UIParams{Contribution: pageUI, Placement: wire.PlacementNavPage, Size: wire.SizeFull, UIVersion: v}, &page); err != nil {
					t.Fatalf("v%d page: %v", v, err)
				}
				raw := checkView(t, &page, false)
				for _, w := range []string{"还没连上你的 openGym", "这一页会显示你在 openGym 里记的训练", settingsPath, serverTitle, tokenTitle,
					"例如 https://gym.example.com", "「设置 › 集成 › API 令牌」", "ogt_", "「查看」", "「应用」"} {
					if !strings.Contains(raw, w) {
						t.Errorf("v%d page has no %q: %s", v, w, raw)
					}
				}
				var badges []string
				for _, s := range page.Sections {
					for _, it := range s.Items {
						if it.Badge != nil {
							badges = append(badges, it.Badge.Text)
						}
					}
				}
				if strings.Join(badges, ",") != c.badges {
					t.Errorf("v%d badges %v, want %s", v, badges, c.badges)
				}
				if strings.Contains(raw, fakeToken) {
					t.Errorf("v%d page shows the key", v)
				}
				var card wire.View
				if err := h.Call("ui.render", wire.UIParams{Contribution: cardUI, Placement: wire.PlacementOverviewCard, Size: wire.SizeCompact, UIVersion: v}, &card); err != nil {
					t.Fatalf("v%d card: %v", v, err)
				}
				raw = checkView(t, &card, true)
				if card.Title != cardTitle || !strings.Contains(raw, "还没连上 openGym") || !strings.Contains(raw, settingsPath) {
					t.Errorf("v%d card %s", v, raw)
				}
				for _, w := range c.card {
					if !strings.Contains(raw, w) {
						t.Errorf("v%d card does not name %q: %s", v, w, raw)
					}
				}
			}
			var st wire.ConnectorStatus
			if err := h.Call("connector.status", wire.ConnectorParams{Contribution: connectorID}, &st); err != nil {
				t.Fatal(err)
			}
			if st.State != wire.ConnectorStateNotConfigured || !strings.Contains(st.Error, settingsPath) || len(st.Guide) != 2 {
				t.Errorf("status %+v", st)
			}
			for _, w := range c.card {
				if !strings.Contains(st.Error, w) {
					t.Errorf("status does not name %q: %s", w, st.Error)
				}
			}
		})
	}
}

func TestToolsAndWritesAnswerWithGuidanceBeforeSetup(t *testing.T) {
	for _, c := range []struct {
		name, server, token string
		want, not           []string
	}{
		{"nothing", "", "", []string{serverTitle, tokenTitle}, nil},
		{"address only", "https://gym.example.com", "", []string{tokenTitle}, []string{serverTitle}},
		{"key only", "", fakeToken, []string{serverTitle}, []string{tokenTitle, fakeToken}},
		{"public http and no key", "http://gym.example.com", "", []string{"用不了", "https", "改好「" + serverTitle + "」", tokenTitle}, nil},
	} {
		a, rt, _ := unconfigured(t, c.server, c.token)
		check := func(what, s string) {
			t.Helper()
			if !strings.Contains(s, "还没连上 openGym") || !strings.Contains(s, settingsPath) {
				t.Errorf("%s: %s: %s", c.name, what, s)
			}
			for _, w := range c.want {
				if !strings.Contains(s, w) {
					t.Errorf("%s: %s does not name %q: %s", c.name, what, w, s)
				}
			}
			for _, w := range c.not {
				if strings.Contains(s, w) {
					t.Errorf("%s: %s names %q: %s", c.name, what, w, s)
				}
			}
		}
		for _, tl := range tools {
			text, err := call(t, a, "tools.call", tl.Name, map[string]any{}, "")
			if err != nil {
				t.Errorf("%s: %s answered an error, not guidance: %v", c.name, tl.Name, err)
				continue
			}
			check(tl.Name, text)
		}
		for _, w := range []struct {
			name string
			args map[string]any
		}{
			{actScheduleDay, map[string]any{"date": "2099-01-01", "rest": true}},
			{actLogWorkout, map[string]any{"exercises": []any{map[string]any{"exercise": "0025", "sets": []any{map[string]any{"reps": 5}}}}}},
			{actCreateRoutine, map[string]any{"name": "上肢 A", "exercises": []any{map[string]any{"exercise": "0025", "sets": 3, "reps": 5}}}},
		} {
			for _, m := range []string{"actions.describe", "actions.do"} {
				_, err := call(t, a, m, w.name, w.args, "r1")
				if err == nil {
					t.Errorf("%s: %s %s ran before setup", c.name, m, w.name)
					continue
				}
				check(m+" "+w.name, err.Error())
			}
		}
		check("status", a.status()["error"].(string))
		if n := rt.count(); n != 0 {
			t.Errorf("%s: %d requests before setup", c.name, n)
		}
	}
}

func TestCheckLoopIsQuietBeforeSetup(t *testing.T) {
	for _, c := range []struct{ name, server, token string }{
		{"nothing", "", ""}, {"address only", "https://gym.example.com", ""}, {"key only", "", fakeToken},
	} {
		a, rt, lines := unconfigured(t, c.server, c.token)
		var namesAt time.Time
		for range 100 { // a hundred rounds: a day and a half at the shortest period
			a.tick(context.Background(), &namesAt)
		}
		if n := rt.count(); n != 0 || len(*lines) != 0 {
			t.Errorf("%s: %d requests and log lines %q while unconfigured", c.name, n, *lines)
		}
		// started as the hub does: one line that says what is missing, and no loop
		b, rt2, lines2 := unconfigured(t, "", "")
		exttest.StartWith(t, manifest, exttest.Serve(t, program(b)), func(p *wire.InitializeParams) {
			p.Config = json.RawMessage(fmt.Sprintf(`{"server_url":%q}`, c.server))
			if c.token != "" {
				p.Secrets = map[string]string{secretToken: c.token}
			}
		})
		b.mu.Lock()
		waiting := b.cfgErr != nil // init returns before it starts the loop
		b.mu.Unlock()
		if !waiting || rt2.count() != 0 || len(*lines2) != 1 || !strings.Contains((*lines2)[0], "not set up yet") {
			t.Errorf("%s: init waiting %v, %d requests, log %q", c.name, waiting, rt2.count(), *lines2)
		}
	}
}
