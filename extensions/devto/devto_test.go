package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

type fakeDEV struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newFakeDEV(t *testing.T) *fakeDEV {
	f := &fakeDEV{hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[r.URL.RequestURI()]++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/articles":
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprint(w, `[{"id":101,"title":"Designing a calmer reading workflow","description":"Keep context with your notes","url":"https://dev.to/example/calmer-reading","comments_count":2,"public_reactions_count":30,"reading_time_minutes":6,"tag_list":["programming","productivity"],"tags":"programming, productivity","user":{"name":"Example Author","username":"author"}},{"id":102,"title":"Other topic","url":"https://dev.to/example/other","comments_count":0,"public_reactions_count":0,"reading_time_minutes":3,"tag_list":["hardware"],"tags":"hardware","user":{"name":"Other","username":"other"}}]`)
		case "/articles/101":
			fmt.Fprint(w, `{"id":101,"title":"Designing a calmer reading workflow","description":"Keep context with your notes","url":"https://dev.to/example/calmer-reading","comments_count":2,"public_reactions_count":30,"reading_time_minutes":6,"tag_list":"programming, productivity","tags":["programming","productivity"],"body_markdown":"# Calmer reading\n\nKeep the original context next to the note.","user":{"name":"Example Author","username":"author"}}`)
		case "/comments":
			fmt.Fprint(w, `[{"id_code":"c1","created_at":"2026-11-09T10:00:00Z","body_html":"<p>Useful context</p><script>never execute</script>","user":{"name":"Reader"},"children":[{"id_code":"c2","created_at":"2026-11-09T11:00:00Z","body_html":"<p>A follow-up</p>","user":{"name":"Another"},"children":[]}]}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDEV) app() *app { return &app{client: newClient(f.srv.URL)} }

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeDEV(t).app())))
}

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "devto" || len(m.Secrets) != 0 {
		t.Fatalf("%+v", m)
	}
	if !slices.Equal(m.Permissions.Network, []string{"dev.to"}) {
		t.Fatal(m.Permissions.Network)
	}
	if len(m.Contributes.Connectors[0].Capabilities) != 2 {
		t.Fatal("public read-only connector expected")
	}
}

func TestInterestsAndArticleRead(t *testing.T) {
	f := newFakeDEV(t)
	a := f.app()
	a.cfg = config{Tags: []string{"programming"}, MinReactions: 10}
	text, err := a.callTool(t.Context(), "devto_articles", json.RawMessage(`{"query":"calmer"}`))
	if err != nil || !strings.Contains(text, "calmer reading") || !strings.Contains(text, "tags: programming, productivity") || strings.Contains(text, "Other topic") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "devto_read", json.RawMessage(`{"id":101}`))
	if err != nil || !strings.Contains(text, "Keep the original context") || !strings.Contains(text, "tags: programming, productivity") || !strings.Contains(text, "Useful context") || !strings.Contains(text, "A follow-up") || strings.Contains(text, "never execute") {
		t.Fatalf("%v %s", err, text)
	}
}

func TestDocumentedTagShapes(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"list", `{"tag_list":["go","webdev"],"tags":"go, webdev"}`, "go,webdev"},
		{"show", `{"tag_list":"go, webdev","tags":["go","webdev"]}`, "go,webdev"},
		{"string only", `{"tags":" go,, webdev , "}`, "go,webdev"},
		{"tag_list string only", `{"tag_list":"go","tags":null}`, "go"},
		{"none", `{"tag_list":"","tags":[]}`, ""},
	} {
		var item article
		if err := json.Unmarshal([]byte(tc.body), &item); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := strings.Join(item.tagNames(), ","); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if tc.want != "" && len(filterArticles([]article{item}, config{Tags: []string{"WebDev", "go"}}, "", 0)) != 1 {
			t.Fatalf("%s: configured tags did not match", tc.name)
		}
	}
	a := newFakeDEV(t).app()
	a.cfg = config{Tags: []string{"productivity"}}
	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
	if err != nil {
		t.Fatal(err)
	}
	if rows := v.Sections[0].Items; len(rows) != 1 || rows[0].Key != "article-101" || !strings.HasSuffix(rows[0].Subtitle, "programming, productivity") {
		t.Fatalf("%+v", v.Sections[0].Items)
	}
}

func TestCacheAvoidsDuplicateFetch(t *testing.T) {
	f := newFakeDEV(t)
	c := newClient(f.srv.URL)
	for range 2 {
		if _, err := c.articles(t.Context(), "", 1); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hits["/articles?page=1&per_page=30"] != 1 {
		t.Fatal(f.hits)
	}
}

func TestStaleReadIsNotFresh(t *testing.T) {
	f := newFakeDEV(t)
	c := newClient(f.srv.URL)
	if _, err := c.articles(t.Context(), "", 1); err != nil {
		t.Fatal(err)
	}
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if _, err := c.get(t.Context(), "/articles?page=1&per_page=30", 0); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatal(err)
	}
}

func TestLocalizedViewsAndDetails(t *testing.T) {
	f := newFakeDEV(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
			v, err := a.render(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			if err := wire.CheckView(v); err != nil {
				t.Fatalf("%s %d: %v", locale, ui, err)
			}
		}
	}
	v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}}, pageState{Mode: "detail", ArticleID: 101})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(v), "never execute") {
		t.Fatal("script was rendered")
	}
}

func TestPageLoadsMoreWithoutReplacingArticles(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[]`)
			return
		}
		items := make([]map[string]any, 30)
		for i := range items {
			items[i] = map[string]any{"id": i + 1, "title": "Article", "url": "https://dev.to/example", "tag_list": []string{"programming"}, "tags": "programming"}
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer server.Close()
	a := &app{client: newClient(server.URL)}
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	v, err := a.render(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.State, r.Action = v.State, "more"
	res, err := a.action(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.View == nil || len(res.View.Sections[0].Items) != 30 {
		t.Fatal("existing articles were replaced by the next page")
	}
	if len(paths) != 2 || paths[1] != "/articles?page=2&per_page=30" {
		t.Fatal(paths)
	}
}

func TestMoreIsOfferedWhileTheLastBatchIsFullEvenIfFiltersHideArticles(t *testing.T) {
	batch := func(n int) []map[string]any {
		items := make([]map[string]any, n)
		for i := range items {
			reactions := 0
			if i == 0 {
				reactions = 40 // the only article the minimum-reactions filter keeps
			}
			items[i] = map[string]any{"id": i + 1, "title": "Article", "url": "https://dev.to/example", "public_reactions_count": reactions, "tag_list": []string{"programming"}}
		}
		return items
	}
	for _, tc := range []struct {
		name     string
		size     int
		wantMore bool
	}{{"full batch", 30, true}, {"short batch", 12, false}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(batch(tc.size))
			}))
			defer server.Close()
			a := &app{client: newClient(server.URL), cfg: config{MinReactions: 10}}
			v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(v.Sections[0].Items); got != 1 {
				t.Fatalf("the filter should keep one article, got %d rows", got)
			}
			if has := v.Sections[0].More != nil; has != tc.wantMore {
				t.Fatalf("More offered = %v, want %v", has, tc.wantMore)
			}
		})
	}
}

func TestDetailBoundsAndStatus(t *testing.T) {
	item := article{ID: 101, Title: "Article", URL: "https://dev.to/example/article", Description: strings.Repeat("summary ", 2000), BodyMarkdown: strings.Repeat("body ", 2000)}
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", ArticleID: 101}, item, nil)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	a := newFakeDEV(t).app()
	if a.status()["state"] != "ok" {
		t.Fatal(a.status())
	}
	if _, err := a.callTool(t.Context(), "devto_articles", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if a.status()["last_sync"] == nil {
		t.Fatal("successful read did not update status")
	}
}
