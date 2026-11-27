package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestCheckViewBoundsForImportDetailAndLongDuplicateSlugs(t *testing.T) {
	// 1. review-import with URL > 1990 chars (<= 2100 allowed by readState) must pass CheckView.
	longURL := "https://example.com/" + strings.Repeat("r", 2000)
	iv := importView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
		pageState{Mode: "review-import", DraftID: "d1", URL: longURL})
	if err := wire.CheckView(iv); err != nil {
		t.Fatalf("importView with long URL failed CheckView: %v", err)
	}

	// 2. detailView with long description, 120 ingredients, and 120 steps must stay within CheckView 300-item limit.
	ings := make([]ingredient, 120)
	for i := range ings {
		ings[i] = ingredient{Title: fmt.Sprintf("Group %d", i+1), Display: fmt.Sprintf("Ingredient %d", i+1)}
	}
	steps := make([]step, 120)
	for i := range steps {
		steps[i] = step{Text: fmt.Sprintf("Step %d instruction", i+1)}
	}
	dv := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
		pageState{Mode: "detail", Slug: "big-recipe"},
		recipe{
			ID:                 "r1",
			Name:               "Grand Feast",
			Slug:               "big-recipe",
			Description:        strings.Repeat("desc line\n", 180),
			OriginalURL:        "https://recipes.example.com/feast",
			RecipeIngredient:   ings,
			RecipeInstructions: steps,
		},
	)
	if err := wire.CheckView(dv); err != nil {
		t.Fatalf("large detailView failed CheckView: %v", err)
	}

	// 3. Recipe list with 180-char slug (valid under slugPattern <= 251) and duplicate slug across pages must pass CheckView.
	longSlug := strings.Repeat("soup-", 36) + "a" // 181 chars
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/recipes" {
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprintf(w, `{"items":[{"id":"1","name":"Soup","slug":%q},{"id":"2","name":"Stew","slug":"stew"}],"page":2,"per_page":25,"total":2,"total_pages":2}`, longSlug)
				return
			}
			fmt.Fprintf(w, `{"items":[{"id":"1","name":"Soup","slug":%q}],"page":1,"per_page":25,"total":2,"total_pages":2}`, longSlug)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	a := &app{client: newClient(srv.URL, "synthetic-mealie-token"), receipts: newWriteStore("")}

	listView, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, State: ext.EncodeState(pageState{Page: 2}),
	}})
	if err != nil {
		t.Fatalf("list render: %v", err)
	}
	if err := wire.CheckView(listView); err != nil {
		t.Fatalf("recipe list with long/duplicate slug failed CheckView: %v", err)
	}
}
