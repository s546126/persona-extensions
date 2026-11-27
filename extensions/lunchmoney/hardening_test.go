package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestCategoriesAndAccountsViewCapsAndWhitespaceCategoryFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/categories":
			cats := make([]category, 0, 320)
			for i := 1; i <= 320; i++ {
				cats = append(cats, category{ID: int64(i), Name: fmt.Sprintf("Cat %d", i)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"categories": cats})
		case "/v2/manual_accounts":
			accs := make([]account, 0, 320)
			for i := 1; i <= 320; i++ {
				accs = append(accs, account{ID: int64(i), Name: fmt.Sprintf("Acct %d", i), Balance: "10.00", Currency: "usd"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"manual_accounts": accs})
		case "/v2/transactions":
			_ = json.NewEncoder(w).Encode(transactionPage{Transactions: []transaction{sampleTransaction()}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := &app{client: newClient(testToken)}
	a.client.http = srv.Client()
	a.client.http.Transport = rewriteHostTransport{base: http.DefaultTransport, host: srv.Listener.Addr().String()}
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	catView, err := a.page(t.Context(), req, pageState{Mode: "categories"})
	if err != nil {
		t.Fatalf("categories page: %v", err)
	}
	if err := wire.CheckView(catView); err != nil {
		t.Fatalf("categories CheckView: %v", err)
	}

	accView, err := a.page(t.Context(), req, pageState{Mode: "accounts", Kind: "manual_accounts"})
	if err != nil {
		t.Fatalf("accounts page: %v", err)
	}
	if err := wire.CheckView(accView); err != nil {
		t.Fatalf("accounts CheckView: %v", err)
	}

	// Whitespace-only category_id in filter action must be treated as empty (0), not rejected.
	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "filter",
		Values: map[string]string{"start_date": "", "end_date": "", "category_id": "   "},
	}})
	if err != nil {
		t.Fatalf("whitespace category_id rejected: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("filter CheckView: %v", err)
	}

	// readState must reject invalid Mode, Kind, or huge Offset.
	for _, bad := range []pageState{
		{Mode: "bogus"},
		{Mode: "accounts", Kind: "invalid"},
		{Mode: "accounts", Kind: ""},
		{Offset: 100000},
	} {
		if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(bad)}}); err == nil {
			t.Errorf("readState accepted invalid state %+v", bad)
		}
	}
}

func TestPaginatedTransactionsDedupAndEmptyHasMoreLoopGuard(t *testing.T) {
	hits := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v2/transactions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		hits++
		mu.Unlock()
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		switch offset {
		case 0:
			_ = json.NewEncoder(w).Encode(transactionPage{
				Transactions: []transaction{
					{ID: 1, Payee: "A", Amount: "1.00", Currency: "usd", Date: "2026-11-10"},
					{ID: 2, Payee: "B", Amount: "2.00", Currency: "usd", Date: "2026-11-10"},
				},
				HasMore: true,
			})
		case 50:
			// Overlapping ID 2 + new ID 3.
			_ = json.NewEncoder(w).Encode(transactionPage{
				Transactions: []transaction{
					{ID: 2, Payee: "B", Amount: "2.00", Currency: "usd", Date: "2026-11-10"},
					{ID: 3, Payee: "C", Amount: "3.00", Currency: "usd", Date: "2026-11-10"},
				},
				HasMore: true,
			})
		default:
			// Empty page claiming has_more=true must stop pagination cleanly.
			_ = json.NewEncoder(w).Encode(transactionPage{Transactions: nil, HasMore: true})
		}
	}))
	defer srv.Close()

	a := &app{client: newClient(testToken)}
	a.client.http = srv.Client()
	a.client.http.Transport = rewriteHostTransport{base: http.DefaultTransport, host: srv.Listener.Addr().String()}

	page, err := a.collectTransactions(t.Context(), "", "", 0, 200)
	if err != nil {
		t.Fatalf("collectTransactions: %v", err)
	}
	if len(page.Transactions) != 3 {
		t.Fatalf("expected 3 deduplicated transactions, got %d: %+v", len(page.Transactions), page.Transactions)
	}
	if page.HasMore {
		t.Fatal("empty batch with has_more=true still reported HasMore=true")
	}
	mu.Lock()
	gotHits := hits
	mu.Unlock()
	if gotHits != 3 {
		t.Fatalf("did not stop on empty batch: made %d requests, want 3", gotHits)
	}
}

func TestConcurrentInitAndReadsNoRace(t *testing.T) {
	a := newApp()
	in := &ext.Init{InitializeParams: wire.InitializeParams{
		Secrets: map[string]string{"api_token": testToken},
	}}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_, _ = a.init(context.Background(), in, nil)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_, _ = a.render(canceledCtx, &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
				_, _ = a.callTool(canceledCtx, "lunchmoney_categories", json.RawMessage(`{}`))
			}
		}()
	}
	wg.Wait()
}
