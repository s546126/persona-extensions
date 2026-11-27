package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

// TestDumpPages writes the page in each state a user can reach as readable text, for reviewing the copy. It runs
// only when RSSHUB_PAGE_DUMP names a directory, and reads nothing but the fake servers of this package's tests.
func TestDumpPages(t *testing.T) {
	dir := os.Getenv("RSSHUB_PAGE_DUMP")
	if dir == "" {
		t.Skip("RSSHUB_PAGE_DUMP is not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, sh := range pageStates(t) {
		if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(viewText(sh.view, sh.toast)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// pageStates draws the page in each state: nothing followed, several sources, one feed and one entry, failing
// sources, previews and wrong input, a wrong or refusing or unreachable instance. Each is checked against the view
// budgets.
func pageStates(t *testing.T) map[string]shown {
	t.Helper()
	ctx := context.Background()
	out := map[string]shown{}
	clock := time.Date(2026, 11, 20, 1, 30, 0, 0, time.UTC)
	keep := func(name string, v *wire.View, toast string) *wire.View {
		checkView(t, v)
		out[name] = shown{v, toast}
		return v
	}
	act := func(a *app, name, action, state string, values map[string]string) *wire.View {
		res, err := a.action(ctx, uiRequest(4, action, state, values))
		if err != nil {
			t.Fatal(err)
		}
		return keep(name, res.View, res.Toast)
	}

	// nothing followed yet
	f := newFakeHub(t)
	a, _, _ := testApp(t, f, config{}, "")
	a.now = func() time.Time { return clock }
	for _, version := range []int{1, 4} {
		v, err := a.render(ctx, uiRequest(version, "", "", nil))
		if err != nil {
			t.Fatal(err)
		}
		keep(fmt.Sprintf("01-empty-v%d", version), v, "")
	}

	// wrong input and a preview of something that is not there
	act(a, "02-preview-bad-input", "preview", "", map[string]string{"route": "hello world"})
	act(a, "03-preview-not-found", "preview", "", map[string]string{"route": "/no/such/route"})
	act(a, "04-preview-ok", "preview", "", map[string]string{"route": "/github/issue/example/repo"})

	// several sources with entries
	for _, s := range []struct{ src, title, tag string }{
		{"/github/issue/example/repo", "Issues", "开发"},
		{"/example/atom", "", "新闻"},
		{"/example/json", "", "新闻"},
	} {
		if _, _, _, err := a.subscribe(ctx, s.src, s.title, s.tag); err != nil {
			t.Fatal(err)
		}
	}
	a.mu.Lock()
	a.lastOK = clock
	a.mu.Unlock()
	v, _ := a.render(ctx, uiRequest(4, "", "", nil))
	keep("05-several-sources", v, "")
	fd, _ := a.feeds().bySource("/github/issue/example/repo")
	v = act(a, "06-one-feed", "feed:"+fd.ID, "", nil)
	act(a, "07-one-entry", "entry:"+fd.Entries[0].ID, v.State, nil)

	// sources failing: one gone (404), one timing out, one blocked
	f.mu.Lock()
	delete(f.feeds, "/example/atom")
	f.mu.Unlock()
	act(a, "08-refresh-with-a-failing-source", "refresh", "", nil)
	js, _ := a.feeds().bySource("/example/json")
	a.feeds().update(js.ID, nil, nil, netError(context.DeadlineExceeded), clock)
	gh, _ := a.feeds().bySource("/github/issue/example/repo")
	a.feeds().update(gh.ID, nil, nil, &httpError{Status: 403, Detail: "Authentication failed. Access denied."}, clock)
	v, _ = a.render(ctx, uiRequest(4, "", "", nil))
	keep("09-sources-failing", v, "")
	atom, _ := a.feeds().bySource("/example/atom")
	act(a, "10-failing-feed", "feed:"+atom.ID, "", nil)
	act(a, "11-subscribe-not-found", "subscribe", "", map[string]string{"route": "/no/such/route"})

	// a wrong instance address in the settings
	b, _, _ := testApp(t, nil, config{InstanceURL: "http://rsshub.example"}, "")
	v, _ = b.render(ctx, uiRequest(4, "", "", nil))
	keep("12-instance-address-wrong", v, "")
	act(b, "13-instance-address-wrong-ping", "ping", "", nil)

	// an instance that refuses (a self-hosted one with an access key not given)
	g := newFakeHub(t)
	g.key = "secret"
	c, _, _ := testApp(t, g, config{}, "")
	act(c, "14-instance-refuses-preview", "preview", "", map[string]string{"route": "/example/atom"})

	// an instance that cannot be reached
	gone := httptest.NewServer(nil)
	gone.Close()
	d, _, _ := testApp(t, nil, config{InstanceURL: gone.URL}, "")
	act(d, "15-instance-unreachable-ping", "ping", "", nil)
	return out
}

// shown is a page as drawn after an action, with the action's toast.
type shown struct {
	view  *wire.View
	toast string
}

// viewText is a view as indented text: sections, then their components with what a user sees on each.
func viewText(v *wire.View, toast string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", v.Title)
	if toast != "" {
		fmt.Fprintf(&b, "[toast] %s\n", toast)
	}
	for _, s := range v.Sections {
		fmt.Fprintf(&b, "\n## %s\n", s.Title)
		for _, c := range s.Items {
			b.WriteString("  " + componentText(c) + "\n")
		}
		if s.Footer != "" {
			fmt.Fprintf(&b, "  (footer) %s\n", s.Footer)
		}
	}
	return b.String()
}

func componentText(c wire.Component) string {
	var p []string
	add := func(label, s string) {
		if s != "" {
			p = append(p, label+s)
		}
	}
	add("", c.Title)
	add("", c.Text)
	add("label: ", c.Label)
	add("subtitle: ", c.Subtitle)
	add("value: ", c.Value)
	add("detail: ", c.Detail)
	add("placeholder: ", c.Placeholder)
	add("style: ", c.Style)
	add("url: ", c.URL)
	if c.Badge != nil {
		add("badge: ", c.Badge.Text)
	}
	for _, s := range c.Stats {
		p = append(p, fmt.Sprintf("stat %s=%s %s", s.Label, s.Value, s.Detail))
	}
	for _, ch := range c.Choices {
		p = append(p, "choice: "+ch.Title)
	}
	if c.Button != nil {
		p = append(p, "button: "+c.Button.Title+" -> "+c.Button.Action.ID)
	}
	if c.Action != nil {
		add("-> ", c.Action.ID)
		add("confirm: ", c.Action.Confirm)
	}
	return "[" + c.Type + "] " + strings.Join(p, " | ")
}
