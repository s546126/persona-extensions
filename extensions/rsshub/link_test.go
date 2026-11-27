package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"
)

// The view check parses a link and refuses the whole view for one that does not parse, has no host or carries user info;
// a link is offered only when it passes, and an entry link that does not is shown as text.
func TestPagesOpenWithALinkThatIsNotACleanHTTPSAddress(t *testing.T) {
	for _, link := range []string{"https://example.com/offers/50%-off", "https://user@example.com/offers/1", "https://example.com:port/offers/1"} {
		f := newFakeHub(t)
		f.set("/example/deals", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Deals</title><link>%[1]s</link>
<item><title>Half price</title><link>%[1]s</link><guid>deal-1</guid><description>Everything must go.</description></item>
</channel></rss>`, link))
		a, _, _ := testApp(t, f, config{}, "")
		ctx := context.Background()
		fd, _, _, err := a.subscribe(ctx, "/example/deals", "", "")
		if err != nil {
			t.Fatalf("link %q: %v", link, err)
		}
		for _, action := range []string{"feed:" + fd.ID, "entry:" + fd.Entries[0].ID} {
			res, err := a.action(ctx, uiRequest(4, action, "", nil))
			if err != nil {
				t.Fatalf("link %q %s: %v", link, action, err)
			}
			if err := wire.CheckView(res.View); err != nil {
				t.Fatalf("link %q %s: %v", link, action, err)
			}
			for _, s := range res.View.Sections {
				for _, c := range s.Items {
					if c.Type == wire.ComponentLink && c.URL != routesDocs { // the form's own link to RSSHub's list of routes
						t.Fatalf("link %q %s: offered %q", link, action, c.URL)
					}
				}
			}
		}
		res, _ := a.action(ctx, uiRequest(4, "entry:"+fd.Entries[0].ID, "", nil))
		if raw := checkView(t, res.View); !strings.Contains(raw, link) {
			t.Fatalf("link %q is not shown as text: %s", link, raw)
		}
	}
}

func TestPagesKeepACleanHTTPSLink(t *testing.T) {
	f := newFakeHub(t)
	a, _, _ := testApp(t, f, config{}, "")
	ctx := context.Background()
	fd, _, _, err := a.subscribe(ctx, "/github/issue/example/repo", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for action, want := range map[string]string{"feed:" + fd.ID: "https://example.com/issues", "entry:" + fd.Entries[0].ID: "https://example.com/issues/2"} {
		res, err := a.action(ctx, uiRequest(4, action, "", nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := wire.CheckView(res.View); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range res.View.Sections {
			for _, c := range s.Items {
				found = found || c.Type == wire.ComponentLink && c.URL == want
			}
		}
		if !found {
			t.Fatalf("%s: the https link is gone", action)
		}
	}
}
