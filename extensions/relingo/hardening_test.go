package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type concurrentTranslator struct{}

func (concurrentTranslator) Translate(context.Context, string, string) (string, error) {
	return "ok", nil
}

func TestManySavedWordsLongGlossAndOversizedStateFitCheckView(t *testing.T) {
	dir := t.TempDir()
	words := make(map[string]savedWord, 320)
	words["lantern"] = savedWord{
		Lemma: "lantern",
		Gloss: strings.Repeat("义", 2500),
		At:    "2026-11-01T00:00:00Z",
	}
	for i := range 319 {
		lemma := fmt.Sprintf("word%c%c", 'a'+i/26, 'a'+i%26)
		words[lemma] = savedWord{Lemma: lemma, Gloss: "n. test", At: "2026-11-01T00:00:00Z"}
	}
	raw, err := json.Marshal(readingState{
		Mode:  modeVocabulary,
		More:  true,
		Words: words,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	a := newApp(t.Logf)
	a.tr = concurrentTranslator{}
	a.store = openStore(dir, t.Logf)

	v, err := a.render(t.Context(), uiReq("", "zh-Hans", 6, nil, pageState{Word: "lantern"}))
	if err != nil {
		t.Fatalf("render with many saved words: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView failed with many saved words and long gloss: %v", err)
	}

	// Oversized state Word must be ignored rather than echoed back into wire.View.State (> 16 KiB).
	oversized, err := a.render(t.Context(), uiReq("", "en", 6, nil, pageState{Word: strings.Repeat("x", 20000)}))
	if err != nil {
		t.Fatalf("render with oversized state: %v", err)
	}
	if err := wire.CheckView(oversized); err != nil {
		t.Fatalf("CheckView failed with oversized state: %v", err)
	}
}

func TestOversizedTranslatorResponseAndBoundedCache(t *testing.T) {
	// Valid JSON prefix followed by >1 MiB padding must be rejected as oversized, not silently truncated.
	payload := `{"responseStatus":200,"responseData":{"translatedText":"ok"}}` + strings.Repeat(" ", 1<<20)
	tr := myMemory{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(payload)),
		}, nil
	})}}
	if _, err := tr.Translate(t.Context(), "hello", "zh-CN"); err == nil {
		t.Fatal("oversized translator response (>1 MiB) was silently accepted")
	}

	s := openStore("", t.Logf)
	for i := range 250 {
		s.rememberTranslation(fmt.Sprintf("zh-CN\npara-%d", i), "译文")
	}
	if got := len(s.snapshot().Translations); got > 200 {
		t.Fatalf("translation cache grew unbounded: got %d entries, want <= 200", got)
	}
}

func TestConcurrentInitAndReadsNoRace(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t.Logf)
	a.tr = concurrentTranslator{}
	in := &ext.Init{InitializeParams: wire.InitializeParams{
		Extension: &wire.ExtensionInfo{DataDir: dir},
		Config:    json.RawMessage(`{"underline_saved":true}`),
	}}

	ctx := context.Background()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				_, _ = a.init(ctx, in, nil)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				_, _ = a.render(ctx, uiReq("", "en", 6, nil, pageState{}))
				_, _ = a.lookupState(ctx, lookupParams{})
				_, _ = a.lookupWord(ctx, lookupParams{Word: "lantern", UILang: "en"})
			}
		}()
	}
	wg.Wait()
}
