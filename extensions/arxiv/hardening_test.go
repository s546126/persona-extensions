package main

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestDuplicatePapersWhitespaceTitleAndLongDetailFieldsFitCheckView(t *testing.T) {
	longTerm := strings.Repeat("cs.", 800)
	longDate := strings.Repeat("2026-", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		if r.URL.Query().Get("id_list") != "" {
			fmt.Fprintf(w, `<feed xmlns="http://www.w3.org/2005/Atom">
			  <entry>
			    <id>http://arxiv.org/abs/2601.01234v1</id>
			    <title>Valid Detail Title</title>
			    <summary>   `+"\n"+`   </summary>
			    <published>%s</published>
			    <category term="%s"/>
			    <author><name>Author One</name></author>
			  </entry>
			</feed>`, longDate, longTerm)
			return
		}
		fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom">
		  <entry>
		    <id>http://arxiv.org/abs/2601.01234v1</id>
		    <title>First Copy</title>
		    <category term="cs.HC"/>
		  </entry>
		  <entry>
		    <id>http://arxiv.org/abs/2601.01234v1</id>
		    <title>Duplicate Copy</title>
		    <category term="cs.HC"/>
		  </entry>
		  <entry>
		    <id>http://arxiv.org/abs/2601.05678v1</id>
		    <title>   `+"\n"+`   </title>
		    <category term="cs.HC"/>
		  </entry>
		</feed>`)
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL)}
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	listView, err := a.page(t.Context(), req, pageState{Query: "context"})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if err := wire.CheckView(listView); err != nil {
		t.Fatalf("list CheckView: %v", err)
	}
	if len(listView.Sections[0].Items) != 1 {
		t.Fatalf("expected 1 deduplicated non-blank paper row, got %d", len(listView.Sections[0].Items))
	}
	if !strings.Contains(listView.Sections[0].Title, "1") {
		t.Fatalf("section title did not reflect rendered count (1): %q", listView.Sections[0].Title)
	}

	detailRes, err := a.page(t.Context(), req, pageState{Mode: "detail", PaperID: "2601.01234v1"})
	if err != nil {
		t.Fatalf("detail page: %v", err)
	}
	if err := wire.CheckView(detailRes); err != nil {
		t.Fatalf("detail CheckView: %v", err)
	}
	if got := detailRes.Sections[1].Items[0].Text; got != textsFor("en").noAbstract {
		t.Fatalf("whitespace summary rendered %q instead of %q", got, textsFor("en").noAbstract)
	}
}

func TestStateValidationMoreOverflowAndDetailIDGuard(t *testing.T) {
	f := newFakeArxiv(t)
	a := &app{client: newClient(f.srv.URL)}

	for _, bad := range []pageState{
		{Mode: "bogus"},
		{Mode: "detail", PaperID: "../bad"},
		{Query: "bad\x00query"},
		{Start: math.MaxInt},
		{Start: maxViewPapers + 20},
	} {
		if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(bad)}}); err == nil {
			t.Errorf("readState accepted invalid state %+v", bad)
		}
	}

	// action("detail") with invalid ID must fail before hitting API.
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "detail", Values: map[string]string{"id": "../etc/passwd"},
	}}); err == nil {
		t.Fatal("detail action accepted invalid paper ID")
	}

	// action("more") at cap must not advance Start past maxViewPapers - 20.
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "more", State: ext.EncodeState(pageState{Query: "reading", Start: maxViewPapers - 20}),
	}}); err == nil {
		t.Fatal("more action allowed advancing past maxViewPapers")
	}

	// queryRange must reject non-positive or oversized count.
	if _, err := a.client.queryRange(t.Context(), "reading", false, 0, 0); err == nil {
		t.Fatal("queryRange accepted count=0")
	}
	if _, err := a.client.queryRange(t.Context(), "reading", false, 0, 101); err == nil {
		t.Fatal("queryRange accepted count=101")
	}
}
