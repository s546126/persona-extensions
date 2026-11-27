package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestConcurrentInitAndReadsNoRace(t *testing.T) {
	f := newFakeSite(t)
	sample(f)
	a := testApp(t, f, Config{})

	initReq := &ext.Init{
		InitializeParams: wire.InitializeParams{
			Config: json.RawMessage(`{"list":"ask","min_score":10}`),
		},
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range 20 {
			_, _ = a.init(context.Background(), initReq, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			_, _ = a.render(context.Background(), req("en", 3, nil, "", nil))
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			_, _ = a.callTool(context.Background(), "hn_stories", json.RawMessage(`{"list":"top"}`))
		}
	}()
	wg.Wait()
}

func TestEmptyTitleAndUnsafeOrOversizedLinkPassCheckView(t *testing.T) {
	f := newFakeSite(t)
	f.set("/topstories.json", "[101,102,103]")
	f.item(map[string]any{
		"id": 101, "type": "story", "by": "alice", "time": testNow - 60,
		"title": "   ", "url": "https://example.com/" + strings.Repeat("a", 2100), "score": 10,
	})
	f.item(map[string]any{
		"id": 102, "type": "story", "by": "bob", "time": testNow - 60,
		"title": "Creds URL", "url": "https://user:pass@example.com/secret", "score": 12,
	})
	f.item(map[string]any{
		"id": 103, "type": "comment", "by": "carol", "time": testNow - 60,
		"title": "", "text": "Direct comment view",
	})
	a := testApp(t, f, Config{})

	listView, err := a.render(context.Background(), req("en", 3, nil, "", nil))
	if err != nil {
		t.Fatalf("list render failed: %v", err)
	}
	checkView(t, listView)

	for _, id := range []int64{101, 102, 103} {
		sv, err := a.render(context.Background(), req("en", 3, pageState{List: "top", Story: id}, "", nil))
		if err != nil {
			t.Fatalf("story %d render failed: %v", id, err)
		}
		checkView(t, sv)
		if strings.TrimSpace(sv.Title) == "" {
			t.Fatalf("story %d produced empty view title", id)
		}
	}
}

func TestDuplicateAndNonPositiveIDsFilteredInStoriesAndThread(t *testing.T) {
	f := newFakeSite(t)
	f.set("/topstories.json", "[1,1,2,0,-5]")
	f.item(map[string]any{"id": 1, "type": "story", "by": "alice", "time": testNow - 60, "title": "One", "score": 10, "kids": []int{11, 11, 0, -1, 13}})
	f.item(map[string]any{"id": 2, "type": "story", "by": "bob", "time": testNow - 60, "title": "Two", "score": 5})
	f.item(map[string]any{"id": 11, "type": "comment", "by": "c1", "time": testNow - 30, "text": "first"})
	f.item(map[string]any{"id": 13, "type": "comment", "by": "c2", "time": testNow - 10, "text": "second"})
	a := testApp(t, f, Config{})

	l, err := a.stories(context.Background(), "top", listMost, false)
	if err != nil {
		t.Fatalf("stories failed: %v", err)
	}
	if len(l.stories) != 2 {
		t.Fatalf("expected 2 deduplicated stories, got %d: %+v", len(l.stories), l.stories)
	}
	if f.hit("/item/0.json") != 0 || f.hit("/item/-5.json") != 0 {
		t.Fatalf("non-positive IDs should not be requested: hit0=%d hitNeg=%d", f.hit("/item/0.json"), f.hit("/item/-5.json"))
	}

	th, err := a.thread(context.Background(), 1, commentsShow, false)
	if err != nil {
		t.Fatalf("thread failed: %v", err)
	}
	if len(th.comments) != 2 {
		t.Fatalf("expected 2 deduplicated comments, got %d: %+v", len(th.comments), th.comments)
	}
}

func TestWhitespaceNullAndBadShapeDoNotPoisonCache(t *testing.T) {
	var itemHits, listHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item/99.json":
			_, _ = w.Write([]byte("  null\n"))
		case "/item/77.json":
			if itemHits.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"error":"temporary"}`))
			} else {
				_, _ = w.Write([]byte(`{"id":77,"type":"story","by":"ann","title":"Recovered"}`))
			}
		case "/topstories.json":
			if listHits.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"error":"temporary"}`))
			} else {
				_, _ = w.Write([]byte(`[77]`))
			}
		default:
			_, _ = w.Write([]byte("null"))
		}
	}))
	t.Cleanup(srv.Close)

	a := newApp()
	a.logf = t.Logf
	a.site = newSite(srv.URL)

	// Whitespace-padded null must be classified as errNotFound, not errBadAnswer / status error.
	if _, err := a.item(context.Background(), 99, false); !errors.Is(err, errNotFound) {
		t.Fatalf("whitespace null err = %v, want errNotFound", err)
	}
	if st := a.status(); st.State != "ok" {
		t.Fatalf("whitespace null poisoned connector status: %+v", st)
	}

	// Wrong-shape JSON must not be cached and must recover on the next call.
	if _, err := a.item(context.Background(), 77, false); !errors.Is(err, errBadAnswer) {
		t.Fatalf("first bad-shape item err = %v, want errBadAnswer", err)
	}
	it, err := a.item(context.Background(), 77, false)
	if err != nil || it.Title != "Recovered" {
		t.Fatalf("second item call failed due to poisoned cache: it=%+v err=%v", it, err)
	}

	if _, err := a.stories(context.Background(), "top", 5, false); !errors.Is(err, errBadAnswer) {
		t.Fatalf("first bad-shape list err = %v, want errBadAnswer", err)
	}
	l, err := a.stories(context.Background(), "top", 5, false)
	if err != nil || len(l.stories) != 1 || l.stories[0].ID != 77 {
		t.Fatalf("second stories call failed due to poisoned cache: l=%+v err=%v", l, err)
	}
}
