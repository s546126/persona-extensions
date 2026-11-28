package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHardeningConcurrentInitAndReadsRace(t *testing.T) {
	a := newApp()
	var init1, init2 ext.Init
	if err := json.Unmarshal([]byte(`{"config":{"hub_url":"https://a.example.com","path":"/j/1"}}`), &init1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"config":{"hub_url":"https://b.example.com","path":"/j/2"}}`), &init2); err != nil {
		t.Fatal(err)
	}
	if _, err := a.init(context.Background(), &init1, nil); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				switch id % 4 {
				case 0:
					in := &init1
					if j%2 == 1 {
						in = &init2
					}
					_, _ = a.init(context.Background(), in, nil)
				case 1:
					_ = a.status()
				case 2:
					_, _ = a.render(context.Background(), &ext.UIRequest{})
				case 3:
					_, _ = a.callTool(context.Background(), "feishu_sidebar_link", json.RawMessage(`{}`))
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestHardeningOversizedLinkPassesCheckView(t *testing.T) {
	longPath := "/j/" + strings.Repeat("对话", 300) // ~1800 bytes -> ~5400 ASCII chars percent-encoded
	a := testApp(t, `{"hub_url":"https://persona.example.com","path":"`+longPath+`"}`)
	v, err := a.render(context.Background(), &ext.UIRequest{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("long link view failed wire.CheckView: %v", err)
	}
	// Ensure no truncated/corrupted Link component was emitted.
	for _, s := range v.Sections {
		for _, it := range s.Items {
			if it.Type == wire.ComponentLink && utf8.RuneCountInString(it.URL) > wire.StringMost {
				t.Fatalf("ComponentLink URL exceeds StringMost: %d", utf8.RuneCountInString(it.URL))
			}
		}
	}
}

func TestHardeningClipRespectsBounds(t *testing.T) {
	if got := clip("hello", 0); got != "" {
		t.Fatalf("clip(hello, 0) = %q, want empty", got)
	}
	if got := clip("hello", -3); got != "" {
		t.Fatalf("clip(hello, -3) = %q, want empty", got)
	}
	if got := clip("hello", 3); utf8.RuneCountInString(got) > 3 {
		t.Fatalf("clip(hello, 3) = %q (%d runes), want <= 3 runes", got, utf8.RuneCountInString(got))
	}
	if got := clip("hello", 1); utf8.RuneCountInString(got) > 1 {
		t.Fatalf("clip(hello, 1) = %q (%d runes), want <= 1 rune", got, utf8.RuneCountInString(got))
	}
}

func TestHardeningRejectsUnsafeBaseAndPath(t *testing.T) {
	for _, badBase := range []string{
		"https://persona.example.com?foo=bar",
		"https://persona.example.com#frag",
		"https://user:secret@persona.example.com",
		"https://:8080",
		"https://persona.example.com\nhttps://evil.com",
	} {
		if got, err := normalizeBase(badBase); err == nil {
			t.Errorf("normalizeBase(%q) succeeded with %q, want error", badBase, got)
		}
	}
	for _, badPath := range []string{
		`/\evil.com`,
		`/j/foo\bar`,
		"/j/%2e%2e/secret",
		"/j/%2E%2E/secret",
		"/j/abc\nhttps://evil.com",
		"/j/abc\r\n",
	} {
		if validPath(badPath) {
			t.Errorf("validPath(%q) = true, want false", badPath)
		}
	}
}
