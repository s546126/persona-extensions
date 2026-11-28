package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona-extensions/pluginkit"
)

func TestHardeningConfigureRaceAndTZReset(t *testing.T) {
	f := newFakeSite(t)
	sample(f)
	p := testPlugin(t, f, map[string]any{"tz": "UTC"})

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range 20 {
			cfg := `{"tz":"UTC","categories":["开发调优"]}`
			if i%2 == 1 {
				cfg = `{"exclude_categories":["前沿快讯"]}`
			}
			_ = p.configure(pluginkit.InitParams{Protocol: 1, Config: json.RawMessage(cfg)})
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			_, _ = p.uiRender(context.Background(), nil, json.RawMessage(`{"contribution":"hot","state":""}`))
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			_, _ = p.cToolsCall(context.Background(), nil, json.RawMessage(`{"name":"linuxdo_hot","args":{}}`))
		}
	}()
	wg.Wait()

	// Clearing tz on re-init must reset p.loc back to time.Local.
	if err := p.configure(pluginkit.InitParams{Protocol: 1, Config: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if p.loc != time.Local {
		t.Fatalf("p.loc after clearing tz = %s, want %s", p.loc, time.Local)
	}
}

func TestHardeningListAndTopicViewBoundsEmptyTitleAndClip(t *testing.T) {
	f := newFakeSite(t)
	var items []item
	for i := range 320 {
		items = append(items, item{
			id:     int64(100 + i),
			title:  fmt.Sprintf("Topic %d", i),
			author: strings.Repeat("a", 1500),
			cat:    strings.Repeat("类", 1500),
			posts:  10,
			people: 5,
			ago:    time.Minute,
		})
	}
	// Blank title + duplicate ID + invalid ID=0.
	items = append(items,
		item{id: 500, title: "   ", author: "bob", cat: "开发", posts: 1, people: 1, ago: time.Minute},
		item{id: 100, title: "Duplicate 100", author: "dup", cat: "开发", posts: 1, people: 1, ago: time.Minute},
		item{id: 0, title: "Invalid zero id", author: "zero", cat: "开发", posts: 1, people: 1, ago: time.Minute},
	)
	f.feeds["/hot.rss"] = rss(items...)

	// Huge config filter to test footer clipping too.
	p := testPlugin(t, f, map[string]any{
		"exclude_keywords": []string{strings.Repeat("屏", 2500)},
	})
	v := render(t, p, "") // calls checkView (<=300 items, <=2000 runes, non-empty titles)
	page := texts(v)
	if strings.Contains(page, "→topic:0") {
		t.Fatalf("topic id 0 must not appear:\n%s", page)
	}

	// Verify blank-title topic in RSS still renders with fallback title and valid checkView.
	f.feeds["/top.rss?period=daily"] = rss(item{id: 500, title: "   ", author: "bob", cat: "开发", posts: 1, people: 1, ago: time.Minute})
	dv, _ := act(t, p, "", "list:daily")
	if !strings.Contains(texts(dv), "帖子 500") {
		t.Fatalf("blank RSS title missing fallback:\n%s", texts(dv))
	}

	// Topic page with blank title, huge username/tags, and zero reply timestamp.
	longTag := strings.Repeat("标", 1500)
	longUser := strings.Repeat("u", 2500)
	rawTopic, _ := json.Marshal(map[string]any{
		"id": 777, "title": "   ", "posts_count": 2, "like_count": 1, "views": 10, "participant_count": 2,
		"tags": []string{longTag, longTag},
		"post_stream": map[string]any{"posts": []map[string]any{
			{"id": 1, "username": longUser, "cooked": "<p>hello</p>", "post_number": 1, "post_type": 1},
			{"id": 2, "username": longUser, "cooked": "<p>reply</p>", "post_number": 2, "post_type": 1, "score": 10},
		}},
	})
	f.topics[777] = string(rawTopic)
	tv, _ := act(t, p, "", "topic:777")
	if tv.Title == "" {
		t.Fatal("topicPage title should have fallback when blank")
	}

	// clip must never exceed n runes and must not panic for n <= 0.
	if got := clip("一二三四五", 3); utf8.RuneCountInString(got) > 3 {
		t.Fatalf("clip(%q, 3) = %q (%d runes > 3)", "一二三四五", got, utf8.RuneCountInString(got))
	}
	if got := clip("hello", 0); got != "" {
		t.Fatalf("clip(hello, 0) = %q, want empty", got)
	}
	if got := clip("hello", -1); got != "" {
		t.Fatalf("clip(hello, -1) = %q, want empty", got)
	}
}

func TestHardeningTopicIDRejectsZero(t *testing.T) {
	for _, raw := range []string{`0`, `"0"`, `"https://linux.do/t/topic/0"`} {
		if id, err := topicID(json.RawMessage(raw)); err == nil {
			t.Errorf("topicID(%s) = %d, want error", raw, id)
		}
	}
}

func TestHardeningBadBodyDoesNotPoisonCacheAndCapsRetryAfter(t *testing.T) {
	f := newFakeSite(t)
	sample(f)
	p := testPlugin(t, f, nil)
	ctx := context.Background()

	// Seed good cache.
	if ts, err := p.hot(ctx, "hot", false); err != nil || len(ts) != 5 {
		t.Fatalf("initial hot: %d %v", len(ts), err)
	}
	// Upstream returns HTTP 200 HTML maintenance page (not RSS).
	f.mu.Lock()
	f.feeds["/hot.rss"] = "<html><body>maintenance</body></html>"
	f.mu.Unlock()

	// Refresh must NOT overwrite the good cached feed with HTML; stale good copy stands in and status becomes error.
	ts, err := p.hot(ctx, "hot", true)
	if err != nil || len(ts) != 5 {
		t.Fatalf("HTML 200 should fall back to cached feed: len=%d err=%v", len(ts), err)
	}
	st, _ := p.cStatus(ctx, nil, nil)
	if s := st.(connectorStatus); s.State != "error" {
		t.Fatalf("status after HTML 200 stand-in: %+v, want error", s)
	}
	// Fresh cached read must still parse cleanly (not poisoned!).
	if ts, err := p.hot(ctx, "hot", false); err != nil || len(ts) != 5 {
		t.Fatalf("cache was poisoned by HTML 200: len=%d err=%v", len(ts), err)
	}

	// Huge Retry-After must be capped at pauseMost (1 hour), not locking out for years.
	srv := newFakeSite(t)
	srv.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "99999999")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	s := newSite(srv.srv.URL)
	_, _ = s.fetch(ctx, "/hot.rss")
	if left := time.Until(s.paused); left > time.Hour+5*time.Second {
		t.Fatalf("Retry-After was not capped: paused for %v", left)
	}
}
