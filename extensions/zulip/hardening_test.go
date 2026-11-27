package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestEmptyAndOversizedTopicsFitCheckView(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/users/me/7/topics":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": "success",
				"topics": []topic{
					{Name: "", MaxID: 100},
					{Name: "   ", MaxID: 99},
					{Name: strings.Repeat("t", 2500), MaxID: 98},
					{Name: "line1\nline2", MaxID: 97},
				},
			})
		case "/api/v1/messages":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result":       "success",
				"found_oldest": true,
				"messages": []message{
					{ID: 100, SenderFullName: "Ann", Subject: "", Content: "Hello empty topic"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testEmail, testToken)}
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: 6}}, pageState{Mode: "topics", StreamID: 7})
		if err != nil {
			t.Fatalf("%s topics: %v", locale, err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("%s topics CheckView: %v", locale, err)
		}
		if len(v.Sections[0].Items) != 4 {
			t.Fatalf("%s expected 4 topic rows, got %d", locale, len(v.Sections[0].Items))
		}
		// Tapping the empty-name topic must succeed and open messages.
		emptyRow := v.Sections[0].Items[0]
		if emptyRow.Action == nil {
			t.Fatalf("%s empty topic missing action", locale)
		}
		res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: locale, UIVersion: 6, State: v.State, Action: "topic", Values: emptyRow.Action.Values,
		}})
		if err != nil {
			t.Fatalf("%s open empty topic: %v", locale, err)
		}
		if err := wire.CheckView(res.View); err != nil {
			t.Fatalf("%s empty topic messages CheckView: %v", locale, err)
		}
	}
}

func TestStreamsCapDedupEmptyNameAndInvalidID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/users/me/subscriptions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		subs := []stream{
			{ID: 0, Name: "invalid zero"},
			{ID: -5, Name: "invalid neg"},
			{ID: 1, Name: "", Description: "empty name stream"},
			{ID: 1, Name: "duplicate stream 1"},
		}
		for i := 2; i <= 320; i++ {
			subs = append(subs, stream{ID: int64(i), Name: fmt.Sprintf("stream-%d", i), Description: "desc"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "success", "subscriptions": subs})
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testEmail, testToken)}
	v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{})
	if err != nil {
		t.Fatalf("streams page: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("streams CheckView: %v", err)
	}
	if len(v.Sections[0].Items) > maxViewItems {
		t.Fatalf("streams count %d exceeds %d", len(v.Sections[0].Items), maxViewItems)
	}
	if v.Sections[0].Items[0].Title == "" {
		t.Fatal("empty stream name produced empty title")
	}
}

func TestFitMessagesCapAnchorLoopStrictParseIDAndRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("anchor") == "42" {
			// Server repeats the anchor with found_oldest=false -> must not loop forever.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result":       "success",
				"found_oldest": false,
				"messages": []message{
					{ID: 42, SenderFullName: "Ann", Subject: "loop", Content: "same anchor"},
				},
			})
			return
		}
		msgs := make([]message, 0, 320)
		for i := 1; i <= 320; i++ {
			msgs = append(msgs, message{ID: int64(i), SenderFullName: "Ann", Subject: "many", Content: fmt.Sprintf("msg %d", i)})
		}
		// Include a duplicate message ID as well.
		msgs = append(msgs, message{ID: 1, SenderFullName: "Ann", Subject: "many", Content: "msg 1 dup"})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result":       "success",
			"found_oldest": true,
			"messages":     msgs,
		})
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testEmail, testToken)}
	v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "messages", StreamID: 7, Topic: "many"})
	if err != nil {
		t.Fatalf("many messages page: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("many messages CheckView: %v", err)
	}
	if len(v.Sections[0].Items) > maxViewItems {
		t.Fatalf("messages items %d exceed %d", len(v.Sections[0].Items), maxViewItems)
	}

	loopView, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "messages", StreamID: 7, Topic: "loop", Anchor: 42})
	if err != nil {
		t.Fatalf("loop view: %v", err)
	}
	if loopView.Sections[0].More != nil {
		t.Fatalf("non-advancing anchor still offered More: %+v", loopView.Sections[0].More)
	}

	for _, bad := range []string{"", "0", "-1", "42abc", " 42", "42 ", "0x10"} {
		if _, err := parsePositiveID(bad); err == nil {
			t.Errorf("parsePositiveID accepted %q", bad)
		}
	}
	for _, bad := range []pageState{
		{Mode: "messages", StreamID: 7, Topic: "bad\ntopic"},
		{Mode: "unknown"},
	} {
		if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(bad)}}); err == nil {
			t.Errorf("readState accepted invalid state %+v", bad)
		}
	}

	// Concurrent init + render/tool calls under -race.
	f := newFakeZulip(t)
	aRace := f.app()
	in := &ext.Init{InitializeParams: wire.InitializeParams{
		Config:  json.RawMessage(fmt.Sprintf(`{"instance_url":%q}`, f.srv.URL)),
		Secrets: map[string]string{"api_email": testEmail, "api_token": testToken},
	}}
	var wg sync.WaitGroup
	ctx := context.Background()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				_, _ = aRace.init(ctx, in, nil)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				_, _ = aRace.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
				_, _ = aRace.callTool(ctx, "zulip_streams", json.RawMessage(`{}`))
			}
		}()
	}
	wg.Wait()
}
