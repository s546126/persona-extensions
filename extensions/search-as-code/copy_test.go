package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"
)

// jargon is wording the pages and settings used to speak in: algorithm terms, English product-internal names and
// protocol details a first-time user cannot place. The pipeline syntax itself (scan_sessions, |>) stays, on the
// advanced tab only, inside code blocks, tables and fields; those are not primary wording and are not checked.
var jargon = []string{
	"算子", "流水线", "Search-as-Code", "Search as Code", "代码化搜索", "CASS", "Perplexity", "fan_out",
	"nodes.call", "Harness", "Without tools", "Step Trace", "证据", "检索", "配方", "命中", "聚合", "已上云",
	"离线缓存", "Agent 会话", "Coding Agent", "（hub）", "hub 上", "hub 所在", "噪音", "索引状态", "workspace",
	"scan_files 算子", "DISKLESSLY_STATE_DIR）", "conversation 默认", " ms",
}

// primaryText is the wording of a view a user reads first: titles, rows, texts, badges, stats, buttons, empty
// states, tabs, the search box and the filter. Code blocks, table cells and field values are left out.
func primaryText(v *wire.View) []string {
	var out []string
	add := func(s ...string) {
		for _, x := range s {
			if strings.TrimSpace(x) != "" {
				out = append(out, x)
			}
		}
	}
	if v == nil {
		return nil
	}
	add(v.Title)
	if v.Search != nil {
		add(v.Search.Placeholder)
	}
	if v.Filter != nil {
		add(v.Filter.Title)
		for _, c := range v.Filter.Choices {
			add(c.Title)
		}
	}
	if v.Tabs != nil {
		for _, t := range v.Tabs.Items {
			add(t.Title)
		}
	}
	for _, sec := range v.Sections {
		add(sec.Title, sec.Footer)
		for _, it := range sec.Items {
			if it.Type == wire.ComponentCode || it.Type == wire.ComponentTable {
				continue
			}
			add(it.Title, it.Subtitle, it.Label)
			if it.Type != wire.ComponentField {
				add(it.Text, it.Detail)
			}
			if it.Badge != nil {
				add(it.Badge.Text)
			}
			for _, b := range it.Badges {
				add(b.Text)
			}
			for _, f := range it.Facts {
				add(f.Label)
			}
			for _, s := range it.Stats {
				add(s.Label, s.Value)
			}
			for _, c := range it.Choices {
				add(c.Title)
			}
			for _, ra := range it.Actions {
				add(ra.Title)
			}
			if it.Button != nil {
				add(it.Button.Title)
			}
		}
	}
	return out
}

func findJargon(t *testing.T, where, text string) {
	t.Helper()
	for _, w := range jargon {
		if strings.Contains(text, w) {
			t.Errorf("%s: jargon %q in %q", where, w, text)
		}
	}
}

// Every page state the fixtures reach speaks plain Chinese in its primary wording.
func TestPagesSpeakPlainChinese(t *testing.T) {
	for _, s := range pageStates(t) {
		for _, line := range primaryText(s.view) {
			findJargon(t, s.name, line)
		}
		if s.err != "" {
			findJargon(t, s.name+" error", s.err)
			if !hasHan(s.err) {
				t.Errorf("%s: error without Chinese: %q", s.name, s.err)
			}
		}
		if s.toast != "" {
			findJargon(t, s.name+" toast", s.toast)
		}
	}
}

// The home page says what it is for (with an example), how to start, and has a separate advanced tab.
func TestHomePageExplainsItself(t *testing.T) {
	var home, empty *wire.View
	for _, s := range pageStates(t) {
		switch s.name {
		case "02-idle-results":
			home = s.view
		case "01-empty-results":
			empty = s.view
		}
	}
	if home == nil || empty == nil {
		t.Fatal("missing states")
	}
	all := strings.Join(primaryText(home), "\n")
	for _, want := range []string{"这是做什么的", "聊天记录", "比如", "搜索框", "Persona 助手", "常用搜索（点一下就搜）"} {
		if !strings.Contains(all, want) {
			t.Errorf("home lacks %q:\n%s", want, all)
		}
	}
	if home.Sections[0].Title != "这是做什么的" {
		t.Errorf("intro is not first: %q", home.Sections[0].Title)
	}
	var tabs []string
	for _, tb := range home.Tabs.Items {
		tabs = append(tabs, tb.Title)
	}
	if strings.Join(tabs, "|") != "结果|常用搜索|搜哪些地方|高级：怎么搜的" {
		t.Errorf("tabs %v", tabs)
	}
	if !strings.Contains(strings.Join(primaryText(empty), "\n"), "还没选要搜哪台电脑") {
		t.Errorf("empty state:\n%s", viewText(empty))
	}
}

// The manifest's name, description, page titles and settings speak plain Chinese too; config keys are unchanged.
func TestManifestSpeaksPlainChinese(t *testing.T) {
	var m struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		Contributes struct {
			UI []struct{ ID, Title string } `json:"ui"`
		} `json:"contributes"`
		Configuration struct {
			Properties map[string]struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				Confirm     string `json:"x-confirm"`
				OneOf       []struct {
					Const, Title string
				} `json:"oneOf"`
			} `json:"properties"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	findJargon(t, "name", m.Name)
	findJargon(t, "description", m.Description)
	for _, tag := range m.Tags {
		findJargon(t, "tag", tag)
	}
	// The hub lists every UI contribution by its title among the extension's switches: the page carries the
	// extension's name, the card the name and 「 · 概况」, so the two never read as a duplicate.
	wantTitle := map[string]string{pageUI: pageTitle, cardUI: cardTitle}
	titles := map[string]bool{}
	for _, u := range m.Contributes.UI {
		findJargon(t, "ui "+u.ID, u.Title)
		if u.Title != wantTitle[u.ID] {
			t.Errorf("ui %s title %q, want %q", u.ID, u.Title, wantTitle[u.ID])
		}
		if titles[u.Title] {
			t.Errorf("two UI contributions are titled %q", u.Title)
		}
		titles[u.Title] = true
	}
	if len(titles) != len(wantTitle) {
		t.Errorf("ui contributions %v, want %v", titles, wantTitle)
	}
	if m.Name != pageTitle {
		t.Errorf("name %q, want %q", m.Name, pageTitle)
	}
	keys := []string{"machines", "session_dirs", "cold_text_dirs", "repo_dirs", "default_scope"}
	if len(m.Configuration.Properties) != len(keys) {
		t.Errorf("settings %v, want %v", m.Configuration.Properties, keys)
	}
	for _, k := range keys {
		p, ok := m.Configuration.Properties[k]
		if !ok {
			t.Errorf("setting %s is gone", k)
			continue
		}
		findJargon(t, k, p.Title+"\n"+p.Description+"\n"+p.Confirm)
		for _, o := range p.OneOf {
			findJargon(t, k+" "+o.Const, o.Title)
			if o.Title != scopeLabels[o.Const] && !strings.HasPrefix(o.Title, scopeLabels[o.Const]) {
				t.Errorf("%s option %s %q does not match the page's %q", k, o.Const, o.Title, scopeLabels[o.Const])
			}
		}
	}
}
