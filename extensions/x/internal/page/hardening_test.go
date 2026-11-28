package page

import (
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/x/internal/connector"
	"github.com/nwverse/persona-extensions/extensions/x/internal/x"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHeaderEmptyAccountLongKeyBadPermalinkAndDuplicateKeysFitCheckView(t *testing.T) {
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	tx := texts("en")
	loc := time.UTC
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	// Empty account in Status must not emit empty Stat.Value (violates minLength: 1).
	p := New(x.New(x.Settings{}, x.Options{}))
	b := &builder{left: componentsMost}
	hdr := p.header(req, tx, x.Overview{
		Status: connector.Status{
			State:   connector.StateError,
			Account: "",
			Error:   "rate limited",
			Guide:   []string{"wait a moment"},
		},
	}, loc, b)

	// Oversized post ID (>128 bytes) and un-linkable permalink (>2000 bytes) plus duplicate post ID.
	longID := strings.Repeat("9", 2100)
	post1 := x.Post{
		ID:        longID,
		Username:  "alice",
		Name:      "Alice",
		Text:      "hello world",
		CreatedAt: now,
	}
	postDup := x.Post{
		ID:        "12345",
		Username:  "bob",
		Name:      "Bob",
		Text:      "duplicate test",
		CreatedAt: now,
	}
	top := wire.Section{Title: tx.top}
	top.Items = append(top.Items, b.post("top", post1, "", tx, loc)...)
	top.Items = append(top.Items, b.post("top", postDup, "", tx, loc)...)
	top.Items = append(top.Items, b.post("top", postDup, "", tx, loc)...)

	// Same check for grokPost with oversized ID and duplicate ID.
	gpost := x.GrokPost{
		ID:     longID,
		Author: "carol",
		Name:   "Carol",
		Text:   "grok summary",
		At:     now,
	}
	gpostDup := x.GrokPost{
		ID:     "g1",
		Author: "dave",
		Name:   "Dave",
		Text:   "dup grok",
		At:     now,
	}
	grokSec := wire.Section{Title: tx.grokPosts}
	grokSec.Items = append(grokSec.Items, b.grokPost("gp", gpost, tx, loc)...)
	grokSec.Items = append(grokSec.Items, b.grokPost("gp", gpostDup, tx, loc)...)
	grokSec.Items = append(grokSec.Items, b.grokPost("gp", gpostDup, tx, loc)...)

	v := &wire.View{
		Title:    "X",
		Sections: []wire.Section{hdr, top, grokSec},
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView failed: %v", err)
	}
}
