package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/budget"
	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/hub"
	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/store"
)

func TestHardeningGatewayEmptyAccountsAndOver100RowsPassCheckView(t *testing.T) {
	// 1. Sources present, zero Accounts -> must not emit 0-row ComponentTable.
	callEmpty := func(ctx context.Context, method string, out any) error {
		*(out.(*hub.GatewayAccounts)) = hub.GatewayAccounts{
			Sources: []hub.GatewaySource{{ID: "src1", Name: "gateway-1", Error: "timeout"}},
		}
		return nil
	}
	a1 := New(filledStore(), budget.Open(""), hub.New("http://hub.example", "tok", callEmpty), fixedNow, func() *time.Location { return loc })
	v1, err := a1.Page(context.Background(), &ext.UIRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(v1); err != nil {
		t.Fatalf("gateway sources-only view failed wire.CheckView: %v", err)
	}

	// 2. >100 Accounts + NaN UsedRatio + >2000-rune fields -> must cap rows <= 100 and pass wire.CheckView.
	nan := math.NaN()
	accounts := make([]hub.GatewayAccount, 120)
	for i := range accounts {
		accounts[i] = hub.GatewayAccount{
			Provider: strings.Repeat("p", 2100),
			Name:     fmt.Sprintf("acct-%d-%s", i, strings.Repeat("n", 2100)),
			Status:   "active",
			Windows:  []hub.GatewayWindow{{Kind: "weekly", UsedRatio: &nan}},
		}
	}
	callMany := func(ctx context.Context, method string, out any) error {
		*(out.(*hub.GatewayAccounts)) = hub.GatewayAccounts{Accounts: accounts}
		return nil
	}
	a2 := New(filledStore(), budget.Open(""), hub.New("http://hub.example", "tok", callMany), fixedNow, func() *time.Location { return loc })
	v2, err := a2.Page(context.Background(), &ext.UIRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(v2); err != nil {
		t.Fatalf("120-account gateway view failed wire.CheckView: %v", err)
	}
}

func TestHardeningOversizedStateChoicesBudgetsAndLongNamesPassCheckView(t *testing.T) {
	st := store.Open("")
	t0, _ := time.ParseInLocation("2006-01-02 15", "2026-11-13 09", loc)
	var buckets []store.Bucket
	for i := 0; i < 160; i++ {
		buckets = append(buckets, store.Bucket{
			Minute:   t0.Unix() + int64(i)*60,
			Model:    fmt.Sprintf("m-%03d-%s", i, strings.Repeat("x", 2100)),
			Launcher: fmt.Sprintf("l-%03d", i),
			Input:    int64(1000 + i),
			Requests: 1,
		})
	}
	// Also include a negative-usage bucket to verify ratio clamping >= 0.
	buckets = append(buckets, store.Bucket{
		Minute: t0.Unix() + 161*60,
		Model:  "neg-model",
		Input:  -500,
	})
	if _, err := st.Merge(strings.Repeat("host", 600), buckets, loc); err != nil {
		t.Fatal(err)
	}

	bf := budget.Open("")
	for i := 0; i < 160; i++ {
		if err := bf.Set(budget.Budget{Span: budget.Daily, Model: fmt.Sprintf("m-%03d", i), Limit: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bf.Set(budget.Budget{Span: budget.Daily, Model: "neg-model", Limit: 1000}); err != nil {
		t.Fatal(err)
	}

	a := New(st, bf, hub.New("http://hub.example", "tok", nil), fixedNow, func() *time.Location { return loc })

	// Oversized incoming page state (> 16 KiB) on all three tabs.
	huge := strings.Repeat("a", 10000)
	for _, tab := range []string{tabOverview, tabDetail, tabBudget} {
		rawState, _ := json.Marshal(pageState{Tab: tab, Machine: huge, Model: huge, Launcher: huge})
		v, err := a.Page(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{State: string(rawState)}})
		if err != nil {
			t.Fatalf("tab %s: %v", tab, err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("tab %s failed wire.CheckView: %v", tab, err)
		}
	}

	// Overview card with >2000-rune top model name must pass wire.CheckView.
	card, err := a.Card(&ext.UIRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(card); err != nil {
		t.Fatalf("card failed wire.CheckView: %v", err)
	}
}
