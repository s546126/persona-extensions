package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHardeningInitConfigRaceAndTZReset(t *testing.T) {
	f := newFakeSite(t)
	sample(f)
	a := testApp(t, f, `{"tz": "UTC", "nodes": ["python"], "list": "nodes"}`)

	// Concurrent re-init and page/tool reads must be -race clean.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range 20 {
			in := &ext.Init{}
			if i%2 == 0 {
				in.InitializeParams.Config = json.RawMessage(`{"tz": "UTC", "nodes": ["python", "go"], "list": "nodes"}`)
			} else {
				in.InitializeParams.Config = json.RawMessage(`{"nodes": ["go"], "list": "hot"}`)
			}
			_, _ = a.init(context.Background(), in, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			_, _ = a.render(context.Background(), req(3, nil))
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			_, _ = a.callTool(context.Background(), "v2ex_hot", json.RawMessage(`{"source": "hot"}`))
		}
	}()
	wg.Wait()

	// Re-initializing without tz must reset a.loc back to time.Local instead of retaining stale UTC.
	in := &ext.Init{}
	in.InitializeParams.Config = json.RawMessage(`{}`)
	if _, err := a.init(context.Background(), in, nil); err != nil {
		t.Fatal(err)
	}
	r := req(3, nil)
	r.TZ = ""
	if got := a.locFor(r); got != time.Local {
		t.Fatalf("loc after clearing tz = %s, want %s", got, time.Local)
	}
}

func TestHardeningListPageCapsItemsAndLongMetaAndReplies(t *testing.T) {
	f := newFakeSite(t)
	var many []fixture
	for i := range 320 {
		many = append(many, fixture{
			id:        int64(5000 + i),
			title:     fmt.Sprintf("Topic %d", i),
			node:      "go",
			nodeTitle: strings.Repeat("节", 1500),
			author:    strings.Repeat("u", 1500),
			replies:   1,
			ago:       time.Minute,
		})
	}
	// Duplicate ID and invalid ID <= 0 must be filtered/deduped cleanly.
	many = append(many, fixture{id: 5000, title: "Duplicate", node: "go", author: "dup"})
	many = append(many, fixture{id: 0, title: "Zero ID", node: "go", author: "zero"})

	f.serve("/topics/hot.json", topicsJSON(many...))
	a := testApp(t, f, `{"exclude_nodes": ["other"]}`)
	v := draw(t, a, 3, nil) // runs wire.CheckView + exttest.CheckView
	total := 0
	for _, s := range v.Sections {
		total += len(s.Items)
	}
	if total > wire.ItemsMost {
		t.Fatalf("total items = %d > %d", total, wire.ItemsMost)
	}
	if findRow(v, "Zero ID") != nil {
		t.Fatalf("topic with id=0 should be filtered out")
	}

	// Long username in reply must not exceed wire.StringMost in topicPage.
	longUser := strings.Repeat("a", 2500)
	f.serve(topicPath(6001), topicsJSON(fixture{id: 6001, title: "Long reply author", node: "go", author: "alice", replies: 1}))
	rawReplies, _ := json.Marshal([]map[string]any{{
		"id": 1, "created": 0, "content": "hi",
		"member": map[string]any{"username": longUser},
	}})
	f.serve(repliesPath(6001), string(rawReplies))
	tv := draw(t, a, 3, &pageState{Topic: 6001})
	if strings.Contains(texts(tv), "01-01 08:00") || strings.Contains(texts(tv), "01-01 00:00") {
		t.Fatalf("zero reply created timestamp rendered epoch date:\n%s", texts(tv))
	}
}

func TestHardeningClipNonPositiveDoesNotPanic(t *testing.T) {
	if got := clip("hello", 0); got != "" {
		t.Fatalf("clip(hello, 0) = %q, want empty", got)
	}
	if got := clip("hello", -5); got != "" {
		t.Fatalf("clip(hello, -5) = %q, want empty", got)
	}
	if got := clip("一二三四五", 3); got != "一二三…" {
		t.Fatalf("existing positive clip contract changed: %q", got)
	}
}

func TestHardeningBadArrayShapeDoesNotPoisonCache(t *testing.T) {
	f := newFakeSite(t)
	sample(f)
	a := testApp(t, f, "")
	ctx := context.Background()

	first, err := a.list(ctx, "hot", false)
	if err != nil || len(first.topics) != 3 {
		t.Fatalf("initial read: %d %v", len(first.topics), err)
	}

	// Upstream returns a syntactically valid JSON array whose elements cannot decode as topics.
	f.put("/topics/hot.json", answer{status: http.StatusOK, body: `[123, "bad"]`})
	l, err := a.list(ctx, "hot", true)
	if err != nil {
		t.Fatalf("expected stale fallback instead of cache-poisoning error: %v", err)
	}
	if !errors.Is(l.stale, errBadAnswer) || len(l.topics) != 3 {
		t.Fatalf("expected stale errBadAnswer with 3 cached topics, got %d stale=%v", len(l.topics), l.stale)
	}

	// Subsequent fresh read must still serve the good cached copy rather than a poisoned [123, "bad"].
	set(t, &listFor, time.Hour)
	again, err := a.list(ctx, "hot", false)
	if err != nil || len(again.topics) != 3 {
		t.Fatalf("cache was poisoned by malformed JSON array: %d %v", len(again.topics), err)
	}

	// Same protection for replies endpoint.
	if _, err := a.thread(ctx, 1001, true, false); err != nil {
		t.Fatalf("initial thread: %v", err)
	}
	f.put(repliesPath(1001), answer{status: http.StatusOK, body: `[{"id": "not-an-int"}]`})
	th, err := a.thread(ctx, 1001, true, true)
	if err != nil || th.repliesErr != nil || len(th.replies) != 25 || !errors.Is(th.stale, errBadAnswer) {
		t.Fatalf("bad replies array should fall back to cached replies: len=%d err=%v repliesErr=%v stale=%v",
			len(th.replies), err, th.repliesErr, th.stale)
	}
}

func TestHardeningOversizedValidJSONRejected(t *testing.T) {
	f := newFakeSite(t)
	sample(f)
	a := testApp(t, f, "")
	// Exactly "[]" padded with spaces past answerMost: previously LimitReader truncated silently and judge accepted it.
	set(t, &answerMost, 16)
	f.put("/topics/hot.json", answer{status: http.StatusOK, body: "[]" + strings.Repeat(" ", 64)})
	_, err := a.list(context.Background(), "hot", true)
	if !errors.Is(err, errBadAnswer) {
		t.Fatalf("oversized body with valid prefix was accepted: %v", err)
	}
	_ = utf8.RuneCountInString("")
}
