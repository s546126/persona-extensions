package main

import (
	"encoding/json"
	"io"
	"log"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/volc/internal/i18n"
	"github.com/nwverse/persona-extensions/extensions/volc/internal/volc"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

// schema is the part of a JSON Schema the settings form uses.
type schema struct {
	Type       string                `json:"type"`
	Default    any                   `json:"default"`
	Pattern    string                `json:"pattern"`
	OneOf      []struct{ Const any } `json:"oneOf"`
	Properties map[string]*schema    `json:"properties"`
}

func readSchema(t *testing.T, raw []byte) *schema {
	t.Helper()
	var s schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func consts(s *schema) []any {
	var out []any
	for _, o := range s.OneOf {
		out = append(out, o.Const)
	}
	return out
}

func authorManifestOf(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(authorManifest, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The package's settings stay within the built-in volc extension's (a frozen copy of the hub's volcConfig, in
// English, testdata/builtin-config.schema.json): no key it lacks, no other type, no choice it does not offer, the same
// defaults and patterns. A hub that rolls the package back to its built-in checks the saved settings against the
// built-in's form (CheckSaved), which then still takes them. Keep the copy frozen until the built-in is retired.
func TestConfigWithinBuiltinSchema(t *testing.T) {
	frozen, err := os.ReadFile("testdata/builtin-config.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	builtin := readSchema(t, frozen)
	pkg := readSchema(t, authorManifestOf(t)["configuration"])
	var within func(at string, p, b *schema)
	within = func(at string, p, b *schema) {
		if b == nil {
			t.Errorf("%s: the built-in has no such setting", at)
			return
		}
		if p.Type != b.Type {
			t.Errorf("%s: type %q, the built-in's is %q", at, p.Type, b.Type)
		}
		if !reflect.DeepEqual(p.Default, b.Default) {
			t.Errorf("%s: default %v, the built-in's is %v", at, p.Default, b.Default)
		}
		if p.Pattern != b.Pattern {
			t.Errorf("%s: pattern %q, the built-in's is %q", at, p.Pattern, b.Pattern)
		}
		if len(b.OneOf) > 0 || len(p.OneOf) > 0 {
			bc := consts(b)
			for _, c := range consts(p) {
				if !slices.Contains(bc, c) {
					t.Errorf("%s: the choice %v is not the built-in's", at, c)
				}
			}
			if len(b.OneOf) > 0 && len(p.OneOf) == 0 {
				t.Errorf("%s: takes any value, the built-in only its choices", at)
			}
		}
		if (p.Properties == nil) != (b.Properties == nil) {
			t.Errorf("%s: an object in one and not the other", at)
		}
		for k, ps := range p.Properties {
			within(at+"."+k, ps, b.Properties[k])
		}
	}
	within("configuration", pkg, builtin)

	// Only the meetings switch is left out: the rest is the built-in's, so the settings carry over both ways.
	var missing []string
	for k := range builtin.Properties {
		if pkg.Properties[k] == nil {
			missing = append(missing, k)
		}
	}
	for k := range builtin.Properties["advanced"].Properties {
		if pkg.Properties["advanced"] == nil || pkg.Properties["advanced"].Properties[k] == nil {
			missing = append(missing, "advanced."+k)
		}
	}
	if !reflect.DeepEqual(missing, []string{"meetings"}) {
		t.Errorf("left out of the built-in's settings: %v, want only meetings", missing)
	}
}

// The form's choices are the program's, in its order, the default first.
func TestConfigMatchesProgram(t *testing.T) {
	pkg := readSchema(t, authorManifestOf(t)["configuration"])
	strs := func(v []any) []string {
		var out []string
		for _, x := range v {
			out = append(out, x.(string))
		}
		return out
	}
	if got := strs(consts(pkg.Properties["voice"])); !slices.Equal(got, volc.Voices) || pkg.Properties["voice"].Default != volc.Voices[0] {
		t.Errorf("voices %v, the program's %v", got, volc.Voices)
	}
	asr := pkg.Properties["advanced"].Properties["asr_model"]
	if got := strs(consts(asr)); !slices.Equal(got, volc.ASRModels) || asr.Default != volc.ASRModels[0] {
		t.Errorf("models %v, the program's %v", got, volc.ASRModels)
	}
	if pkg.Properties["read_answers"].Default != true {
		t.Error("read_answers is on unless turned off")
	}
}

// The manifest is the built-in's identity: the same id, contributions, page, secret and network, a program on every
// platform serving HTTP, the secret required, and a hub that has the module-provider handoff.
func TestManifest(t *testing.T) {
	var m wire.Manifest
	if err := json.Unmarshal(authorManifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != volc.ExtensionID || m.Version != version || version == "" || m.Publisher != "nwverse" {
		t.Errorf("identity %q %q %q", m.ID, m.Version, m.Publisher)
	}
	if m.Engines.ExtensionProtocol != 1 || m.Engines.Persona != ">=202611272113" {
		t.Errorf("engines %+v", m.Engines)
	}
	if m.Runtime == nil || !m.Runtime.HTTP {
		t.Fatalf("runtime %+v", m.Runtime)
	}
	var cmd map[string]string
	raw, _ := json.Marshal(m.Runtime.Command)
	if err := json.Unmarshal(raw, &cmd); err != nil {
		t.Fatalf("runtime.command: %v", err)
	}
	for _, p := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		if cmd[p] != "bin/volc-"+p {
			t.Errorf("runtime.command[%s] = %q", p, cmd[p])
		}
	}
	if len(cmd) != 4 {
		t.Errorf("runtime.command %v", cmd)
	}
	if !slices.Equal(m.ActivationEvents, []string{"on_startup"}) || !m.SettingsCheck {
		t.Errorf("activation %v, settings_check %v", m.ActivationEvents, m.SettingsCheck)
	}
	if m.Permissions == nil || !slices.Equal(m.Permissions.Network, []string{"openspeech.bytedance.com"}) {
		t.Errorf("permissions %+v", m.Permissions)
	}
	c := m.Contributes
	if len(c.Transcription) != 1 || c.Transcription[0].ID != volc.TranscriptionContribution ||
		len(c.Speech) != 1 || c.Speech[0].ID != volc.SpeechContribution {
		t.Errorf("transcription %+v, speech %+v", c.Transcription, c.Speech)
	}
	if len(c.UI) != 1 || c.UI[0].ID != volc.StatusPage || c.UI[0].Placement != "extension_page" {
		t.Errorf("ui %+v", c.UI)
	}
	if len(m.Secrets) != 1 || m.Secrets[0].Name != volc.SecretKey || string(m.Secrets[0].Hint) != `"last4"` || !m.Secrets[0].Required {
		t.Errorf("secrets %+v", m.Secrets)
	}
	for _, a := range c.API {
		for _, f := range a.Features {
			if f == "modules" || strings.HasPrefix(f, "module.") {
				t.Errorf("api %s claims the host's feature %q", a.ID, f)
			}
		}
	}
	// The runtime manifest leaves out the source-only icon.
	var packaged map[string]json.RawMessage
	if err := json.Unmarshal(manifest, &packaged); err != nil {
		t.Fatal(err)
	}
	if _, ok := packaged["icon"]; ok {
		t.Error("the runtime manifest has the icon")
	}
	if _, err := url.Parse(m.Homepage); err != nil || !strings.HasPrefix(m.Homepage, "https://") {
		t.Errorf("homepage %q", m.Homepage)
	}
}

// The program keeps the process protocol: initialize answers the manifest's id and version, the status page draws
// for every client, unknown methods are refused, and it ends when the hub closes its stdin.
func TestProtocol(t *testing.T) {
	pl := volc.New(volc.Form{}, "", i18n.ZhHans, log.New(io.Discard, "", 0))
	exttest.Check(t, authorManifest, exttest.Serve(t, volc.Program(version, manifest, pl)))
}
