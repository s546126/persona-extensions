package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func startLookup(t *testing.T, a *app, config ...json.RawMessage) *exttest.Host {
	t.Helper()
	return exttest.StartWith(t, manifest, exttest.Serve(t, program(a)), func(p *wire.InitializeParams) {
		p.Extension.DataDir = a.store.pathDir()
		p.Plugin.DataDir = p.Extension.DataDir
		if len(config) > 0 {
			p.Config = config[0]
		}
	})
}

func lookupCall(t *testing.T, h *exttest.Host, method string, params any) json.RawMessage {
	t.Helper()
	var res json.RawMessage
	if err := h.Call("text_lookup."+method, params, &res); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return res
}

func TestLookupStateSaveRemove(t *testing.T) {
	a := testApp(t, &staticTranslator{text: "x"})
	h := startLookup(t, a)
	var state stateResult
	if err := json.Unmarshal(lookupCall(t, h, "state", map[string]any{"contribution": "relingo"}), &state); err != nil {
		t.Fatal(err)
	}
	if state.UnderlineSaved || len(state.Words) != 0 {
		t.Fatalf("default state: %+v", state)
	}
	var saved savedResult
	if err := json.Unmarshal(lookupCall(t, h, "save", map[string]any{"contribution": "relingo", "word": "Harbors", "ui_lang": "zh-Hans"}), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Saved {
		t.Fatal("save did not keep the word")
	}
	if err := json.Unmarshal(lookupCall(t, h, "state", map[string]any{"contribution": "relingo"}), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Words) != 1 || state.Words[0] != "harbor" {
		t.Fatalf("state words: %+v", state.Words)
	}
	// The reading page sees the same list.
	if a.store.snapshot().Words["harbor"].Lemma != "harbor" {
		t.Fatal("reading store did not get the lookup save")
	}
	if err := json.Unmarshal(lookupCall(t, h, "remove", map[string]any{"contribution": "relingo", "word": "harbor"}), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Saved {
		t.Fatal("remove left the word saved")
	}
	if len(a.savedLemmas()) != 0 {
		t.Fatalf("words left: %v", a.savedLemmas())
	}
}

func TestLookupWord(t *testing.T) {
	a := testApp(t, &staticTranslator{text: "x"})
	a.store.save("harbor", "n. 海港")
	h := startLookup(t, a)
	var found lookupResult
	raw := lookupCall(t, h, "lookup", map[string]any{
		"contribution": "relingo", "word": "harbors", "context": "the harbors", "lang": "en", "ui_lang": "zh-Hans",
	})
	if err := json.Unmarshal(raw, &found); err != nil {
		t.Fatal(err)
	}
	if !found.Found || found.Lemma != "harbor" || !found.Saved || len(found.Senses) == 0 || found.Senses[0].Text == "" {
		t.Fatalf("zh lookup: %+v", found)
	}
	raw = lookupCall(t, h, "lookup", map[string]any{"contribution": "relingo", "word": "harbor", "ui_lang": "en"})
	if err := json.Unmarshal(raw, &found); err != nil {
		t.Fatal(err)
	}
	if !found.Found || found.Senses[0].Text == "" || strings.Contains(found.Senses[0].Text, "海港") {
		t.Fatalf("en gloss: %+v", found)
	}
	var missing lookupResult
	raw = lookupCall(t, h, "lookup", map[string]any{"contribution": "relingo", "word": "notawordxyz", "lang": "en", "ui_lang": "en"})
	if err := json.Unmarshal(raw, &missing); err != nil {
		t.Fatal(err)
	}
	if missing.Found || missing.Lemma != "" || len(missing.Senses) != 0 {
		t.Fatalf("unknown word: %+v", missing)
	}
	var notSaved savedResult
	if err := json.Unmarshal(lookupCall(t, h, "save", map[string]any{"contribution": "relingo", "word": "notawordxyz"}), &notSaved); err != nil {
		t.Fatal(err)
	}
	if notSaved.Saved {
		t.Fatal("unknown word was saved")
	}
}

func TestUnderlineSavedDefault(t *testing.T) {
	a := testApp(t, &staticTranslator{text: "x"})
	h := startLookup(t, a)
	var state stateResult
	if err := json.Unmarshal(lookupCall(t, h, "state", map[string]any{"contribution": "relingo"}), &state); err != nil {
		t.Fatal(err)
	}
	if state.UnderlineSaved {
		t.Fatal("underline_saved defaulted to true")
	}
	on := testApp(t, &staticTranslator{text: "x"})
	init := &ext.Init{InitializeParams: wire.InitializeParams{Protocol: ext.Protocol}}
	init.InitializeParams.Config = []byte(`{"underline_saved":true}`)
	if _, err := on.init(context.Background(), init, nil); err != nil {
		t.Fatal(err)
	}
	if !on.underlineSaved {
		t.Fatal("underline_saved true was ignored")
	}
	var manifestSettings struct {
		Configuration struct {
			Properties struct {
				UnderlineSaved struct {
					Type    string `json:"type"`
					Default *bool  `json:"default"`
				} `json:"underline_saved"`
			} `json:"properties"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(authorManifest, &manifestSettings); err != nil {
		t.Fatal(err)
	}
	field := manifestSettings.Configuration.Properties.UnderlineSaved
	if field.Type != "boolean" || field.Default == nil || *field.Default {
		t.Fatalf("setting: %+v", field)
	}
	_ = h
}

func TestLookupInvalidParams(t *testing.T) {
	a := testApp(t, &staticTranslator{text: "x"})
	h := startLookup(t, a)
	var raw json.RawMessage
	err := h.Call("text_lookup.lookup", json.RawMessage(`{"contribution":"relingo","word":`), &raw)
	if err == nil {
		t.Fatal("truncated params were accepted")
	}
	err = h.Call("text_lookup.lookup", map[string]any{"contribution": "other", "word": "harbor"}, &raw)
	if err == nil {
		t.Fatal("wrong contribution was accepted")
	}
	err = h.Call("text_lookup.nope", map[string]any{"contribution": "relingo"}, &raw)
	if err == nil {
		t.Fatal("unknown method was accepted")
	}
}

func TestLookupRemoveBlankOrUnknownIsNotSaved(t *testing.T) {
	a := testApp(t, &staticTranslator{text: "x"})
	h := startLookup(t, a)
	for _, word := range []string{"", "   ", "qzxv"} {
		var res savedResult
		if err := json.Unmarshal(lookupCall(t, h, "remove", map[string]any{"contribution": "relingo", "word": word}), &res); err != nil {
			t.Fatal(err)
		}
		if res.Saved {
			t.Fatalf("remove %q reports saved: true", word)
		}
	}
}

// A saved word the candidate rules keep from being offered (here developer words) still answers a lookup, as the
// state underlines it, and its inflected form removes it; an unsaved one stays unoffered.
func TestLookupSavedDeveloperWord(t *testing.T) {
	a := testApp(t, &staticTranslator{text: "x"})
	a.store.save("kernel", "n. kernel")
	a.store.save("compile", "v. compile")
	h := startLookup(t, a)
	var got lookupResult
	if err := json.Unmarshal(lookupCall(t, h, "lookup", map[string]any{"contribution": "relingo", "word": "kernel", "ui_lang": "en"}), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.Lemma != "kernel" || !got.Saved {
		t.Fatalf("saved developer word: %+v", got)
	}
	got = lookupResult{}
	if err := json.Unmarshal(lookupCall(t, h, "lookup", map[string]any{"contribution": "relingo", "word": "syntax", "ui_lang": "en"}), &got); err != nil {
		t.Fatal(err)
	}
	if got.Found {
		t.Fatalf("unsaved developer word is offered: %+v", got)
	}
	var res savedResult
	if err := json.Unmarshal(lookupCall(t, h, "remove", map[string]any{"contribution": "relingo", "word": "compiles"}), &res); err != nil {
		t.Fatal(err)
	}
	if _, still := a.store.snapshot().Words["compile"]; still || res.Saved {
		t.Fatalf("removing compiles left compile saved (saved=%v)", res.Saved)
	}
	// A saved developer word or non-lexicon entry is also removed when passed with uppercase letters.
	a.store.save("endpoint", "n. endpoint")
	a.store.save("customterm", "n. customterm")
	for _, tc := range []struct{ surface, lemma string }{
		{"Endpoint", "endpoint"},
		{"CustomTerm", "customterm"},
	} {
		res = savedResult{}
		if err := json.Unmarshal(lookupCall(t, h, "remove", map[string]any{"contribution": "relingo", "word": tc.surface}), &res); err != nil {
			t.Fatal(err)
		}
		if _, still := a.store.snapshot().Words[tc.lemma]; still || res.Saved {
			t.Fatalf("removing %q left %q saved (saved=%v)", tc.surface, tc.lemma, res.Saved)
		}
	}
}

func TestLookupSaveEmptyGlossIsNotSaved(t *testing.T) {
	a := testApp(t, &staticTranslator{text: "x"})
	h := startLookup(t, a)
	// An ECDICT entry can have neither an English nor a Chinese gloss; the store keeps nothing for it.
	const word = "harbor"
	old, had := lexicon.lemmas[word]
	lexicon.lemmas[word] = gloss{}
	t.Cleanup(func() {
		if had {
			lexicon.lemmas[word] = old
		} else {
			delete(lexicon.lemmas, word)
		}
	})
	var res savedResult
	if err := json.Unmarshal(lookupCall(t, h, "save", map[string]any{"contribution": "relingo", "word": word, "ui_lang": "en"}), &res); err != nil {
		t.Fatal(err)
	}
	if res.Saved {
		t.Fatal("save reported an empty-gloss word as saved")
	}
	if _, kept := a.store.snapshot().Words[word]; kept {
		t.Fatal("store kept an empty-gloss word")
	}
	// Saving a word that is already saved still answers saved.
	lexicon.lemmas[word] = old
	for range 2 {
		res = savedResult{}
		if err := json.Unmarshal(lookupCall(t, h, "save", map[string]any{"contribution": "relingo", "word": word, "ui_lang": "en"}), &res); err != nil {
			t.Fatal(err)
		}
		if !res.Saved {
			t.Fatal("saving a real word did not report saved")
		}
	}
}
