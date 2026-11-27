package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestMinScoreJSONTagAcceptedByStoriesTool(t *testing.T) {
	f := newFakeLobsters(t)
	a := f.app()
	text, err := a.callTool(t.Context(), "lobsters_stories", json.RawMessage(`{"filter":"hot","min_score":10}`))
	if err != nil {
		t.Fatalf("min_score argument rejected: %v", err)
	}
	if !strings.Contains(text, "Why local-first matters") || strings.Contains(text, "different topic") {
		t.Fatalf("min_score filter did not apply: %s", text)
	}
}

func TestListAndDetailViewCheckViewBoundsDedupAndState(t *testing.T) {
	longTag := strings.Repeat("tag", 800) // 2400 chars > wire.StringMost
	longUser := strings.Repeat("u", 2500)
	longURL := "https://example.com/" + strings.Repeat("a", 2500)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/hottest.json":
			list := []story{
				{ShortID: "dup1", Title: "", Score: 10, Tags: []string{longTag}},
				{ShortID: "dup1", Title: "Duplicate story", Score: 10, Tags: []string{"go"}},
				{ShortID: "../bad", Title: "Invalid ID", Score: 10, Tags: []string{"go"}},
			}
			for i := 1; i <= 310; i++ {
				list = append(list, story{
					ShortID:      fmt.Sprintf("s%d", i),
					Title:        fmt.Sprintf("Story %d", i),
					Score:        i,
					CommentCount: 1,
					Tags:         []string{"go"},
				})
			}
			_ = json.NewEncoder(w).Encode(list)
		case "/s/detail1.json":
			_ = json.NewEncoder(w).Encode(story{
				ShortID:          "detail1",
				Title:            strings.Repeat("T", 2500),
				URL:              longURL,
				CommentsURL:      longURL,
				Score:            5,
				CommentCount:     2,
				Submitter:        longUser,
				Tags:             []string{longTag},
				DescriptionPlain: "\n\n\n",
				Comments: []comment{
					{ShortID: "c1", User: longUser, Score: 1, Depth: 0, Text: "hello"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL)}
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	listView, err := a.page(t.Context(), req, pageState{})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if err := wire.CheckView(listView); err != nil {
		t.Fatalf("list CheckView: %v", err)
	}
	if len(listView.Sections[0].Items) > maxViewItems {
		t.Fatalf("list items %d exceed cap %d", len(listView.Sections[0].Items), maxViewItems)
	}

	detailRes, err := a.page(t.Context(), req, pageState{Mode: "detail", StoryID: "detail1"})
	if err != nil {
		t.Fatalf("detail page: %v", err)
	}
	if err := wire.CheckView(detailRes); err != nil {
		t.Fatalf("detail CheckView: %v", err)
	}

	// Han search query of 250 runes must be clipped to <= 200 runes and survive readState on subsequent render.
	searchRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "search", Values: map[string]string{"q": strings.Repeat("字", 250)},
	}})
	if err != nil {
		t.Fatalf("han search: %v", err)
	}
	if err := wire.CheckView(searchRes.View); err != nil {
		t.Fatalf("han search CheckView: %v", err)
	}
	st, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: searchRes.View.State}})
	if err != nil || utf8.RuneCountInString(st.Query) > 200 {
		t.Fatalf("han query did not round-trip in readState: st=%+v err=%v", st, err)
	}

	// Invalid filter or mode in state must be rejected by readState instead of failing CheckView.
	for _, bad := range []pageState{
		{Filter: "bogus"},
		{Mode: "bogus"},
		{Mode: "detail", StoryID: "../bad"},
	} {
		if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(bad)}}); err == nil {
			t.Errorf("readState accepted invalid state %+v", bad)
		}
	}
}

func TestConcurrentInitAndReadsNoRace(t *testing.T) {
	f := newFakeLobsters(t)
	a := f.app()
	in := &ext.Init{InitializeParams: wire.InitializeParams{
		Config: json.RawMessage(`{"tags":["programming"],"min_score":1}`),
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
				_, _ = a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
				_, _ = a.callTool(ctx, "lobsters_stories", json.RawMessage(`{}`))
			}
		}()
	}
	wg.Wait()
}
