package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestConcurrentInitAndReadsNoDataRace(t *testing.T) {
	f := newFakeDEV(t)
	a := f.app()

	stop := make(chan struct{})
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			cfgRaw := json.RawMessage(`{"tags":["programming"],"min_reactions":0}`)
			if i%2 == 1 {
				cfgRaw = json.RawMessage(`{"tags":["go"],"min_reactions":5}`)
			}
			_, _ = a.init(t.Context(), &ext.Init{InitializeParams: wire.InitializeParams{Config: cfgRaw}}, nil)
		}
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 30 {
			_, _ = a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
		}
	}()
	go func() {
		defer wg.Done()
		for range 30 {
			_, _ = a.callTool(t.Context(), "devto_articles", json.RawMessage(`{}`))
		}
	}()
	wg.Wait()
	close(stop)
	writerWG.Wait()
}

func TestUnicodeQueryAndTagRuneLimits(t *testing.T) {
	f := newFakeDEV(t)
	a := f.app()

	// 100-rune CJK query is 300 bytes (> 200 bytes) but <= 200 runes: must be accepted.
	cjkQuery := strings.Repeat("技", 100)
	if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(pageState{Query: cjkQuery})}}); err != nil {
		t.Fatalf("readState rejected 100-rune CJK query: %v", err)
	}
	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "search",
		Values:    map[string]string{"q": cjkQuery},
	}})
	if err != nil {
		t.Fatalf("search 100-rune CJK query: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("CheckView(search CJK): %v", err)
	}

	// >200 runes must be rejected in search action too.
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "search",
		Values:    map[string]string{"q": strings.Repeat("a", 201)},
	}}); err == nil {
		t.Fatal("search accepted query > 200 runes")
	}

	// 30-rune CJK tag is 90 bytes (> 60 bytes) but <= 60 runes: must be accepted by devto_articles.
	cjkTag := strings.Repeat("标", 30)
	args, _ := json.Marshal(map[string]any{"tag": cjkTag})
	if _, err := a.callTool(t.Context(), "devto_articles", args); err != nil {
		t.Fatalf("devto_articles rejected 30-rune CJK tag: %v", err)
	}
}

func TestListDeduplicatesAcrossPagesAndClipsSubtitle(t *testing.T) {
	longTag := strings.Repeat("tag", 800)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/articles" {
			page := r.URL.Query().Get("page")
			var items []article
			if page == "1" {
				for i := range 30 {
					items = append(items, article{
						ID:    int64(100 + i),
						Title: "Article",
						URL:   "https://dev.to/example/a",
						Tags:  json.RawMessage(`["go"]`),
					})
				}
			} else {
				// Page 2 repeats ID 129 (live feed shift), has empty title, and huge tag subtitle.
				items = []article{
					{ID: 129, Title: "Shifted duplicate", URL: "https://dev.to/example/dup"},
					{ID: 200, Title: "", URL: "https://dev.to/example/empty", Tags: json.RawMessage(`["` + longTag + `"]`)},
				}
			}
			_ = json.NewEncoder(w).Encode(items)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL)}
	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Page: 1}),
	}})
	if err != nil {
		t.Fatalf("render list page 1: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(list): %v", err)
	}
}

func TestDetailViewEmptyCommentAuthorAndLongNames(t *testing.T) {
	item := article{
		ID:           101,
		Title:        "",
		URL:          "https://dev.to/example/article",
		BodyMarkdown: "Body text",
		User:         struct{ Name, Username string }{Name: strings.Repeat("Author", 500)},
	}
	comments := []comment{
		{ID: "c1", BodyHTML: "<p>Anonymous comment</p>", User: struct{ Name string }{Name: ""}},
		{ID: "c2", BodyHTML: "<p>Long author</p>", User: struct{ Name string }{Name: strings.Repeat("R", 2500)}},
	}
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", ArticleID: 101}, item, comments)
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(detailView): %v", err)
	}
}

func TestUnexpectedJSONShapeNotCached(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if hits.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"error":"transient object shape"}`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":101,"title":"Recovered","url":"https://dev.to/example/ok"}]`))
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	if _, err := c.articles(t.Context(), "", 1); err == nil {
		t.Fatal("first call with object JSON should fail")
	}
	items, err := c.articles(t.Context(), "", 1)
	if err != nil {
		t.Fatalf("second call after bad JSON shape should not be poisoned by cache: %v", err)
	}
	if len(items) != 1 || items[0].ID != 101 {
		t.Fatalf("items = %+v, want [101]", items)
	}
}
