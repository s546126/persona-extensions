package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestInvalidTabModePreferOversizedStateAndPlacesCapFitCheckView(t *testing.T) {
	f := newFake(t)
	a, _ := testApp(t, f)

	// Invalid tab value in action must not produce Tabs.Value outside Tabs.Items.
	res, err := a.actionPage(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "tab",
		Values: map[string]string{"tab": "bogus"},
	}})
	if err != nil {
		t.Fatalf("bogus tab action: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("bogus tab CheckView: %v", err)
	}

	// Huge keywords/city inputs must be clipped so View.State, Field.Value, and empty-result titles pass CheckView.
	f.queue("/v5/place/text", `{"status":"1","infocode":"10000","pois":[]}`)
	huge := strings.Repeat("北", 4000)
	searchRes, err := a.actionPage(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "search",
		Values: map[string]string{"keywords": huge, "city": huge},
	}})
	if err != nil {
		t.Fatalf("huge search action: %v", err)
	}
	if err := wire.CheckView(searchRes.View); err != nil {
		t.Fatalf("huge search CheckView: %v", err)
	}

	// Invalid mode and prefer values must normalize back to valid choices.
	routeRes, err := a.actionPage(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "route",
		Values: map[string]string{"mode": "spaceship", "prefer": "bogus", "from": huge, "to": ""},
	}})
	if err != nil {
		t.Fatalf("invalid route options: %v", err)
	}
	if err := wire.CheckView(routeRes.View); err != nil {
		t.Fatalf("invalid route CheckView: %v", err)
	}
	gotState := readUIState(&ext.UIRequest{UIParams: wire.UIParams{State: routeRes.View.State}})
	if gotState.Mode != "driving" || gotState.Prefer != "recommended" {
		t.Fatalf("invalid mode/prefer not normalized: mode=%q prefer=%q", gotState.Mode, gotState.Prefer)
	}

	// >300 saved places must be capped so the places tab stays within wire.ItemsMost.
	a.placesMu.Lock()
	a.uiPlaces = make([]fixedPlace, 0, 310)
	for i := range 310 {
		a.uiPlaces = append(a.uiPlaces, fixedPlace{
			Name:   fmt.Sprintf("地点%d", i+1),
			Target: fmt.Sprintf("39.%04d,116.%04d", 9000+i%100, 3000+i%100),
			Source: "ui",
		})
	}
	a.placesMu.Unlock()
	placesView, err := a.renderPage(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		State: ext.EncodeState(uiState{Tab: tabPlaces}),
	}})
	if err != nil {
		t.Fatalf("places page: %v", err)
	}
	if err := wire.CheckView(placesView); err != nil {
		t.Fatalf("places CheckView with 310 places: %v", err)
	}
}

func TestConcurrentInitAndReadsNoRace(t *testing.T) {
	f := newFake(t)
	a := newApp()
	a.probe = false
	a.tune = func(c *client) {
		c.base = f.srv.URL
		c.http = f.srv.Client()
		c.gap = 0
	}
	in := &ext.Init{InitializeParams: wire.InitializeParams{
		Secrets: map[string]string{keySecret: "test-key-32chars-0000000000000000"},
		Config:  json.RawMessage(`{}`),
	}}

	ctx := context.Background()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_, _ = a.init(ctx, in, nil)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_ = a.status()
				_, _ = a.renderCard(ctx, &ext.UIRequest{})
				_, _ = a.renderPage(ctx, &ext.UIRequest{})
			}
		}()
	}
	wg.Wait()
}
