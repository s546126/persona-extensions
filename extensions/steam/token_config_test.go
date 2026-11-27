package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"
)

func fixtureTokenConfig(t *testing.T, fields string) TokenConfig {
	t.Helper()
	var cfg config
	raw := fmt.Sprintf(`{"steam_id":%q,"country_code":" gb " %s}`, fixtureID, fields)
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg, issue := cfg.normalized()
	if issue != "" || cfg.SteamID != fixtureID || cfg.CountryCode != "GB" {
		t.Fatalf("legacy account and country settings failed: %+v, issue=%s", cfg, issue)
	}
	return cfg.TokenConfig
}

func TestTokenConfigDefaultsAndDisabledLimits(t *testing.T) {
	defaults := fixtureTokenConfig(t, "")
	if defaults.AllowsInboxEvents() || defaults.InboxEventLimit() != 0 || defaults.AllowsPatchNotesSummary() || defaults.AllowsSaleRoundup() ||
		defaults.AllowsMemoryExport() || defaults.MemoryItemLimit() != 0 || !defaults.AllowsDiscoveryTools() || defaults.DiscoveryResultLimit() != 15 {
		t.Fatalf("unexpected token config defaults: %+v", defaults)
	}
	configured := fixtureTokenConfig(t, `,"inbox_max_events_per_day":35,"sale_roundup_enabled":true,"memory_max_items":22,"discovery_tools_enabled":false,"discovery_max_results":40`)
	if configured.AllowsSaleRoundup() || configured.InboxEventLimit() != 0 || configured.MemoryItemLimit() != 0 ||
		configured.AllowsDiscoveryTools() || configured.DiscoveryResultLimit() != 0 {
		t.Fatalf("a configured value bypassed its feature gate: %+v", configured)
	}
	patchOnly := fixtureTokenConfig(t, `,"patch_notes_summary_enabled":true`)
	if !patchOnly.AllowsPatchNotesSummary() || patchOnly.AllowsInboxEvents() || patchOnly.AllowsSaleRoundup() {
		t.Fatal("enabling patch summaries also changed the inbox permissions")
	}
}

func TestTokenConfigEnabledBudgetBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fields  string
		budgets [3]int
	}{
		{"missing", "", [3]int{5, 10, 15}},
		{"below or above range", `,"inbox_max_events_per_day":0,"memory_max_items":31,"discovery_max_results":-1`, [3]int{5, 10, 15}},
		{"minimum", `,"inbox_max_events_per_day":1,"memory_max_items":1,"discovery_max_results":1`, [3]int{1, 1, 1}},
		{"maximum", `,"inbox_max_events_per_day":50,"memory_max_items":30,"discovery_max_results":50`, [3]int{50, 30, 50}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureTokenConfig(t, `,"inbox_events_enabled":true,"sale_roundup_enabled":true,"patch_notes_summary_enabled":true,"memory_export_enabled":true,"discovery_tools_enabled":true`+tc.fields)
			if !cfg.AllowsInboxEvents() || !cfg.AllowsMemoryExport() || !cfg.AllowsDiscoveryTools() ||
				!cfg.AllowsSaleRoundup() || !cfg.AllowsPatchNotesSummary() {
				t.Fatalf("explicit opt-ins were lost: %+v", cfg)
			}
			got := [3]int{cfg.InboxEventLimit(), cfg.MemoryItemLimit(), cfg.DiscoveryResultLimit()}
			if got != tc.budgets {
				t.Fatalf("effective budgets %v, want %v", got, tc.budgets)
			}
		})
	}
}

func TestTokenSettingsManifestAndTranslations(t *testing.T) {
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	var form struct {
		Required   []string
		Properties map[string]struct {
			Type, Title, Description string
			Default                  json.RawMessage
			Minimum, Maximum         int
		}
	}
	if err := json.Unmarshal(m.Configuration, &form); err != nil {
		t.Fatal(err)
	}
	defaults := fixtureTokenConfig(t, `,"inbox_events_enabled":true,"memory_export_enabled":true`)
	fields := map[string]struct {
		defaultJSON string
		maximum     int
	}{
		"inbox_events_enabled":        {"false", 0},
		"inbox_max_events_per_day":    {strconv.Itoa(defaults.InboxEventLimit()), 50},
		"patch_notes_summary_enabled": {"false", 0},
		"sale_roundup_enabled":        {"false", 0},
		"memory_export_enabled":       {"false", 0},
		"memory_max_items":            {strconv.Itoa(defaults.MemoryItemLimit()), 30},
		"discovery_tools_enabled":     {"true", 0},
		"discovery_max_results":       {strconv.Itoa(defaults.DiscoveryResultLimit()), 50},
	}
	for name, want := range fields {
		field := form.Properties[name]
		if field.Title == "" || field.Description == "" || string(field.Default) != want.defaultJSON || slices.Contains(form.Required, name) {
			t.Errorf("missing or inconsistent form field %s: %+v", name, field)
		}
		if want.maximum == 0 && field.Type != "boolean" ||
			want.maximum != 0 && (field.Type != "integer" || field.Minimum != 1 || field.Maximum != want.maximum) {
			t.Errorf("invalid form type or limits for %s: %+v", name, field)
		}
	}
	for _, lang := range []string{"en", "zh-Hans"} {
		raw, err := os.ReadFile(filepath.Join("l10n", lang+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var translation struct {
			Configuration map[string]struct{ Title, Description string }
		}
		if err := json.Unmarshal(raw, &translation); err != nil {
			t.Fatal(err)
		}
		for name := range fields {
			if field := translation.Configuration[name]; field.Title == "" || field.Description == "" {
				t.Errorf("missing %s translation for %s", lang, name)
			}
		}
	}
}
