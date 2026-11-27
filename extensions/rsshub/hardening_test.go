package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHomePageCapsFeedGroupsWithinCheckViewLimit(t *testing.T) {
	a, _, _ := testApp(t, nil, config{}, "")
	st := a.feeds()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for i := range 280 {
		tag := fmt.Sprintf("分组%d", i%10)
		src := fmt.Sprintf("/example/feed/%d", i)
		f, added, err := st.add(src, fmt.Sprintf("网站 %d", i), tag, now)
		if err != nil || !added {
			t.Fatalf("add %d failed: added=%v err=%v", i, added, err)
		}
		if i < 40 {
			st.update(f.ID, nil, &parsedFeed{
				Title: f.Title,
				Entries: []parsedEntry{{
					Key:       fmt.Sprintf("k-%d", i),
					Title:     fmt.Sprintf("文章 %d", i),
					Published: now.Add(time.Duration(i) * time.Minute),
				}},
			}, nil, now)
		}
	}

	v := a.page(&ext.UIRequest{UIParams: wire.UIParams{UIVersion: 4}}, pageState{})
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("home page with 280 feeds failed CheckView: %v", err)
	}
	for _, sec := range v.Sections {
		speaksPlainly(t, "capped-home section", sec.Title+" "+sec.Footer)
		for _, c := range sec.Items {
			speaksPlainly(t, "capped-home "+c.Type, c.Title+" "+c.Text+" "+c.Subtitle)
		}
	}
}

func TestLongLinksAndOversizedPreviewStatePassCheckView(t *testing.T) {
	a, _, _ := testApp(t, nil, config{}, "")
	st := a.feeds()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	longURL := "https://example.com/" + strings.Repeat("a", 2100)

	f, _, err := st.add("/example/long", "Long Site", "新闻", now)
	if err != nil {
		t.Fatal(err)
	}
	st.update(f.ID, nil, &parsedFeed{
		Title: "Long Site",
		Link:  longURL,
		Entries: []parsedEntry{{
			Key:       "k-long",
			Title:     "Long Link Entry",
			Link:      longURL,
			Published: now,
		}},
	}, nil, now)

	gotFeed, _ := st.get(f.ID)
	entryID := gotFeed.Entries[0].ID

	feedView := a.page(&ext.UIRequest{UIParams: wire.UIParams{UIVersion: 4}}, pageState{Feed: f.ID})
	if err := wire.CheckView(feedView); err != nil {
		t.Fatalf("feed view with >2000-char SiteLink failed CheckView: %v", err)
	}

	entryView := a.page(&ext.UIRequest{UIParams: wire.UIParams{UIVersion: 4}}, pageState{
		Entry:   entryID,
		Preview: strings.Repeat("p", 2500),
	})
	if err := wire.CheckView(entryView); err != nil {
		t.Fatalf("entry view with >2000-char link and oversized Preview state failed CheckView: %v", err)
	}
}

func TestStatusReportsErrorWhenInstanceFailing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Access denied"))
	}))
	t.Cleanup(srv.Close)

	a := newApp(t.Logf)
	a.configure(config{InstanceURL: srv.URL}, "")
	if err := a.ping(context.Background()); err == nil {
		t.Fatal("expected ping to fail on 403")
	}
	st := a.status()
	if st["state"] != "error" {
		t.Fatalf("status state = %v when instance failing, want error (full=%+v)", st["state"], st)
	}
	if st["error"] == "" {
		t.Fatalf("expected non-empty status error: %+v", st)
	}
}

func TestActionsDoScopedPerAppRejectsMismatchAndDedupesConcurrent(t *testing.T) {
	var hits atomic.Int32
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			hits.Add(1)
			<-gate
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>%s</title><entry><id>1</id><title>Hello</title></entry></feed>`, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	a1 := newApp(t.Logf)
	a1.configure(config{InstanceURL: srv.URL}, "")
	a2 := newApp(t.Logf)
	a2.configure(config{InstanceURL: srv.URL}, "")

	// 1. Two independent app instances must not share request_id receipts.
	w1, err := a1.readWrite(actSubscribe, json.RawMessage(`{"route":"/alpha"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a1.do(context.Background(), "shared-req-id", w1); err != nil {
		t.Fatalf("a1 subscribe failed: %v", err)
	}
	w2, err := a2.readWrite(actSubscribe, json.RawMessage(`{"route":"/beta"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a2.do(context.Background(), "shared-req-id", w2); err != nil {
		t.Fatalf("a2 subscribe failed: %v", err)
	}
	if _, ok := a2.feeds().bySource("/beta"); !ok {
		t.Fatal("a2 did not execute subscribe because receipt bled from a1's global done map")
	}

	// 2. Reusing the same request_id on the same app with DIFFERENT params must fail.
	w1Diff, err := a1.readWrite(actSubscribe, json.RawMessage(`{"route":"/gamma"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a1.do(context.Background(), "shared-req-id", w1Diff); err == nil {
		t.Fatal("reusing request_id with different route unexpectedly succeeded")
	}

	// 3. Concurrent duplicate actions.do with same request_id must execute only once.
	wSlow1, err := a1.readWrite(actSubscribe, json.RawMessage(`{"route":"/slow"}`))
	if err != nil {
		t.Fatal(err)
	}
	wSlow2, err := a1.readWrite(actSubscribe, json.RawMessage(`{"route":"/slow"}`))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = a1.do(context.Background(), "concurrent-req-id", wSlow1)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = a1.do(context.Background(), "concurrent-req-id", wSlow2)
	}()
	time.Sleep(40 * time.Millisecond)
	close(gate)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent duplicate do failed: %v", errs)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("concurrent duplicate actions.do ran upstream %d times, want 1", got)
	}
}

func TestOpenStoreDedupesFeedsAndIsolatesTagSlices(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	disk := diskState{
		Version: 1,
		Feeds: []*feed{
			{ID: "f1", Source: "/a", Added: now, Entries: []entry{{ID: "f1-1", Key: "k1", Title: "T1", Tags: []string{"go"}}}},
			{ID: "f1", Source: "/a-dup", Added: now},
		},
	}
	raw, _ := json.Marshal(disk)
	if err := os.WriteFile(filepath.Join(dir, stateFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	st := openStore(dir, t.Logf)
	list := st.list()
	if len(list) != 1 {
		t.Fatalf("expected 1 deduplicated feed, got %d", len(list))
	}
	list[0].Entries[0].Tags[0] = "mutated"
	again, _ := st.get("f1")
	if again.Entries[0].Tags[0] != "go" {
		t.Fatalf("copyFeed aliased entry.Tags slice: got %q, want go", again.Entries[0].Tags[0])
	}
}
