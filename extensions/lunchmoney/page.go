package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Mode       string `json:"m,omitempty"`
	StartDate  string `json:"s,omitempty"`
	EndDate    string `json:"e,omitempty"`
	CategoryID int64  `json:"c,omitempty"`
	Offset     int    `json:"o,omitempty"`
	Kind       string `json:"k,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Offset < 0 || st.Offset > maxViewOffset || st.CategoryID < 0 || len(st.StartDate) > 20 || len(st.EndDate) > 20 || len(st.Mode) > 30 || len(st.Kind) > 30 {
		return pageState{}, errors.New("invalid page state")
	}
	switch st.Mode {
	case "", "categories":
		if st.Kind != "" {
			return pageState{}, errors.New("invalid page state")
		}
	case "accounts":
		if st.Kind != "manual_accounts" && st.Kind != "plaid_accounts" {
			return pageState{}, errors.New("invalid page state")
		}
	default:
		return pageState{}, errors.New("invalid page state")
	}
	return st, nil
}

func (a *app) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	return a.page(ctx, r, st)
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	switch r.Action {
	case "filter":
		st.StartDate, st.EndDate, st.Offset = strings.TrimSpace(r.Values["start_date"]), strings.TrimSpace(r.Values["end_date"]), 0
		catRaw := strings.TrimSpace(r.Values["category_id"])
		if catRaw == "" {
			st.CategoryID, err = 0, nil
		} else {
			st.CategoryID, err = strconv.ParseInt(catRaw, 10, 64)
		}
		if err != nil || st.CategoryID < 0 {
			return nil, errors.New("invalid category ID")
		}
	case "more":
		st.Offset, err = strconv.Atoi(r.Values["offset"])
		if err != nil || st.Offset < 50 || st.Offset > maxViewOffset {
			return nil, errors.New("invalid next offset")
		}
	case "categories":
		st = pageState{Mode: "categories"}
	case "accounts":
		if r.Values["kind"] != "manual_accounts" && r.Values["kind"] != "plaid_accounts" {
			return nil, errors.New("invalid account type")
		}
		st = pageState{Mode: "accounts", Kind: r.Values["kind"]}
	case "back":
		st.Mode, st.Kind = "", ""
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	return &wire.ActionResult{View: v}, err
}

// maxViewRecords is half the SDK's component limit: a transaction renders a line and a notes line.
const (
	maxViewRecords = 125
	maxViewItems   = 250
	maxViewOffset  = 500
)

// collectTransactions reads the batches from the start through the one at offset so that Load more answers the whole
// list, longer. It stops at maxViewRecords to keep the view within the SDK's component limit, and HasMore reports
// whether another batch can be loaded.
func (a *app) collectTransactions(ctx context.Context, start, end string, categoryID int64, offset int) (transactionPage, error) {
	cl := a.currentClient()
	var out transactionPage
	seen := make(map[int64]struct{})
	for pos := 0; pos <= offset && len(out.Transactions) < maxViewRecords; pos += 50 {
		batch, err := cl.transactions(ctx, start, end, categoryID, pos)
		if err != nil {
			return transactionPage{}, err
		}
		added := 0
		for _, tx := range batch.Transactions {
			if tx.ID != 0 {
				if _, dup := seen[tx.ID]; dup {
					continue
				}
				seen[tx.ID] = struct{}{}
			}
			out.Transactions = append(out.Transactions, tx)
			added++
		}
		if added == 0 {
			out.HasMore = false
			break
		}
		out.HasMore = batch.HasMore
		if !batch.HasMore {
			break
		}
	}
	if len(out.Transactions) > maxViewRecords {
		out.Transactions = out.Transactions[:maxViewRecords]
	}
	if len(out.Transactions) >= maxViewRecords {
		out.HasMore = false
	}
	return out, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	cl := a.currentClient()
	if cl.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "categories" {
		items, err := cl.categories(ctx)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.categories}
		seen := make(map[int64]struct{}, min(len(items), maxViewItems))
		for _, item := range items {
			if item.ID != 0 {
				if _, dup := seen[item.ID]; dup {
					continue
				}
				seen[item.ID] = struct{}{}
			}
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%s · %d", item.Name, item.ID), 1800)})
			if len(section.Items) >= maxViewItems {
				break
			}
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noCategories})
		}
		return &wire.View{Title: t.categories, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}}}}, nil
	}
	if st.Mode == "accounts" {
		items, err := cl.accounts(ctx, st.Kind)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.accounts}
		seen := make(map[int64]struct{}, min(len(items), maxViewItems))
		for _, item := range items {
			if item.ID != 0 {
				if _, dup := seen[item.ID]; dup {
					continue
				}
				seen[item.ID] = struct{}{}
			}
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%s · %s %s · %s", item.Name, item.Balance, item.Currency, item.asOf()), 1800)})
			if len(section.Items) >= maxViewItems {
				break
			}
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noAccounts})
		}
		return &wire.View{Title: t.accounts, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}}}}, nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid finance view")
	}
	page, err := a.collectTransactions(ctx, st.StartDate, st.EndDate, st.CategoryID, st.Offset)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.finance, State: ext.EncodeState(st)}
	section := wire.Section{Title: t.transactions}
	for _, item := range page.Transactions {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%s · %s %s · %s", item.Payee, item.Amount, item.Currency, item.Date), 1800)})
		if item.Notes != "" {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(item.Notes, 1800), Style: wire.TextStyleSecondary})
		}
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noTransactions})
	}
	if page.HasMore {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"offset": fmt.Sprint(st.Offset + 50)}}}
	}
	v.Sections = []wire.Section{{Title: t.filter, Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "start_date", Label: t.start, Value: st.StartDate},
		{Type: wire.ComponentField, Kind: "text", Name: "end_date", Label: t.end, Value: st.EndDate},
		{Type: wire.ComponentField, Kind: "text", Name: "category_id", Label: t.category, Value: categoryText(st.CategoryID)},
		{Type: wire.ComponentButton, Title: t.apply, Action: &wire.Action{ID: "filter", Submit: true}},
		{Type: wire.ComponentButton, Title: t.categories, Action: &wire.Action{ID: "categories", Push: &wire.Push{Title: t.categories, State: ext.EncodeState(pageState{Mode: "categories"})}}},
	}}, section, {Title: t.accounts, Items: []wire.Component{
		{Type: wire.ComponentButton, Title: t.manual, Action: &wire.Action{ID: "accounts", Values: map[string]string{"kind": "manual_accounts"}, Push: &wire.Push{Title: t.manual, State: ext.EncodeState(pageState{Mode: "accounts", Kind: "manual_accounts"})}}},
		{Type: wire.ComponentButton, Title: t.synced, Action: &wire.Action{ID: "accounts", Values: map[string]string{"kind": "plaid_accounts"}, Push: &wire.Push{Title: t.synced, State: ext.EncodeState(pageState{Mode: "accounts", Kind: "plaid_accounts"})}}},
	}}}
	return v, nil
}

func categoryText(id int64) string {
	if id == 0 {
		return ""
	}
	return fmt.Sprint(id)
}
