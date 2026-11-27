package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// termwise 0.1.1 wrote reading.json in the extension data directory. The market
// rename keeps that directory; relingo must load every saved word unchanged.
func TestTermwiseDataDirLoadsInRelingo(t *testing.T) {
	dir := t.TempDir()
	const legacy = `{` + "\n" + `  "mode": "vocabulary",` + "\n" + `  "more": true,` + "\n" + `  "words": {` + "\n" + `    "harbor": {"lemma": "harbor", "gloss": "n. 海港", "saved_at": "2024-06-01T00:00:00Z"},` + "\n" + `    "lantern": {"lemma": "lantern", "gloss": "n. 灯笼", "saved_at": "2024-06-02T00:00:00Z"}` + "\n" + `  }` + "\n" + `}` + "\n"
	path := filepath.Join(dir, stateFile)
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openStore(dir, func(string, ...any) {})
	snap := s.snapshot()
	if snap.Mode != modeVocabulary || !snap.More {
		t.Fatalf("mode/more changed: %+v", snap)
	}
	if len(snap.Words) != 2 {
		t.Fatalf("words: %+v", snap.Words)
	}
	for _, lemma := range []string{"harbor", "lantern"} {
		w, ok := snap.Words[lemma]
		if !ok || w.Lemma != lemma || w.Gloss == "" || w.At == "" {
			t.Fatalf("%s: %+v ok=%v", lemma, w, ok)
		}
	}
	if snap.Words["harbor"].Gloss != "n. 海港" || snap.Words["lantern"].At != "2024-06-02T00:00:00Z" {
		t.Fatalf("saved fields changed: %+v", snap.Words)
	}
	// A later save must not drop the words termwise wrote.
	s.save("merchant", "n. 商人")
	again := openStore(dir, func(string, ...any) {})
	got := again.snapshot()
	for _, lemma := range []string{"harbor", "lantern", "merchant"} {
		if got.Words[lemma].Lemma != lemma {
			t.Fatalf("after save, %s missing: %+v", lemma, got.Words)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded readingState
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Words["harbor"].Gloss != "n. 海港" {
		t.Fatalf("file lost the termwise gloss: %s", raw)
	}
}

func TestEmbeddedManifestStripsPredecessor(t *testing.T) {
	var source, runtime map[string]json.RawMessage
	if err := json.Unmarshal(authorManifest, &source); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifest, &runtime); err != nil {
		t.Fatal(err)
	}
	if string(source["replaces"]) != `"termwise"` {
		t.Fatalf("predecessor: %s", source["replaces"])
	}
	for _, key := range []string{"tags", "replaces", "icon"} {
		if _, ok := runtime[key]; ok {
			t.Fatalf("author metadata %s leaked into runtime manifest", key)
		}
	}
	var identity struct{ ID, Version string }
	if err := json.Unmarshal(manifest, &identity); err != nil {
		t.Fatal(err)
	}
	if identity.ID != "relingo" || identity.Version != "0.2.1" {
		t.Fatalf("identity %+v", identity)
	}
}
