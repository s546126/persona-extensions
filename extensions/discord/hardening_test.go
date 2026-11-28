package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHardeningGuildsAndReadBackAndReadAfterLoopsTerminate(t *testing.T) {
	// 1. Guilds endpoint repeating 200 guilds with same last ID must terminate and dedup.
	guildCalls := 0
	page200 := make([]map[string]any, 200)
	for i := range 200 {
		page200[i] = map[string]any{"id": fmt.Sprintf("%d", 1000+i), "name": fmt.Sprintf("G%d", i)}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v10/users/@me/guilds":
			guildCalls++
			if guildCalls > 10 {
				w.WriteHeader(500)
				return
			}
			writeJSON(w, 200, page200)
		case r.URL.Path == "/api/v10/channels/100/messages":
			// 100 messages with stagnant IDs (oldest does not move before anchor, newest does not advance past anchor).
			msgs := make([]map[string]any, 100)
			for i := range 100 {
				msgs[i] = map[string]any{
					"id":         "500000000000000000",
					"channel_id": "100",
					"type":       0,
					"content":    "stagnant",
					"author":     map[string]any{"id": "1", "username": "u"},
					"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
				}
			}
			writeJSON(w, 200, msgs)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	a := newAPI(srv.URL+"/api/v10", "tok", "0.1.2", srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	gs, err := a.guilds(ctx)
	if err != nil {
		t.Fatalf("guilds: %v", err)
	}
	if guildCalls > 2 || len(gs) != 200 {
		t.Fatalf("guilds did not break cycle/dedup: calls=%d len=%d", guildCalls, len(gs))
	}

	c := newConn(endpoints{API: srv.URL + "/api/v10", Web: "https://discord.example"}, t.Logf)
	s := &session{api: a}

	// 2. readBack with stagnant oldest ID must terminate immediately instead of looping.
	_, _, err = c.readBack(ctx, s, 100, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("readBack: %v", err)
	}

	// 3. readAfter with stagnant newest ID (<= anchor) must terminate and report more=false.
	_, _, more, err := c.readAfter(ctx, s, 100, 500000000000000000)
	if err != nil {
		t.Fatalf("readAfter: %v", err)
	}
	if more {
		t.Fatal("readAfter reported more=true despite zero forward progress")
	}
}

func TestHardeningZeroArchivedAtDoesNotCorruptCursorUpto(t *testing.T) {
	f := newFakeDiscord(t)
	g := f.addGuild("Example Server")
	forum := f.addChannel(g, "ideas", chForum)
	now := time.Now()
	thID := f.id(now.Add(-2 * time.Hour))
	msgID := f.id(now.Add(-time.Hour))
	validThID := f.id(now.Add(-3 * time.Hour))
	validMsgID := f.id(now.Add(-90 * time.Minute))

	f.set(func() {
		f.messages[thID] = []map[string]any{{
			"id":         msgID.String(),
			"channel_id": thID.String(),
			"type":       0,
			"content":    "hello",
			"author":     f.user(f.ownerID, "Owner", false),
			"timestamp":  now.Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		}}
		f.messages[validThID] = []map[string]any{{
			"id":         validMsgID.String(),
			"channel_id": validThID.String(),
			"type":       0,
			"content":    "after-nometa",
			"author":     f.user(f.ownerID, "Owner", false),
			"timestamp":  now.Add(-90 * time.Minute).UTC().Format(time.RFC3339Nano),
		}}
	})
	// Custom server wrapper returning has_more=true with a nil-metadata thread followed by a normal archived thread.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/threads/archived/public") {
			writeJSON(w, 200, map[string]any{
				"threads": []any{
					map[string]any{
						"id":              thID.String(),
						"type":            chPublicThread,
						"guild_id":        g.id.String(),
						"parent_id":       forum.String(),
						"name":            "NoMeta",
						"last_message_id": msgID.String(),
					},
					map[string]any{
						"id":              validThID.String(),
						"type":            chPublicThread,
						"guild_id":        g.id.String(),
						"parent_id":       forum.String(),
						"name":            "AfterNoMeta",
						"last_message_id": validMsgID.String(),
						"thread_metadata": map[string]any{
							"archived":          true,
							"archive_timestamp": now.Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano),
						},
					},
				},
				"has_more": true,
			})
			return
		}
		f.serve(w, r)
	}))
	t.Cleanup(srv.Close)

	c := newConn(endpoints{API: srv.URL + "/api/v10", Web: "https://discord.example"}, t.Logf)
	c.healthEvery = time.Hour
	c.budget = time.Second
	h := exttest.Start(t, manifest, exttest.Serve(t, c.program(version)))
	<-c.ready
	var pr wire.ConnectorPrompt
	if err := h.Call(ext.ConnectorNamespace+".login.step", wire.ConnectorLoginParams{
		Contribution: connectorID, Step: "token", Values: map[string]string{"token": fakeToken},
	}, &pr); err != nil || !pr.Done {
		t.Fatalf("signIn: %+v %v", pr, err)
	}

	var res wire.ConnectorChangesResult
	if err := h.Call(ext.ConnectorNamespace+".archive.changes", wire.ConnectorChangesParams{
		Contribution: connectorID, Limit: 20,
	}, &res); err != nil {
		t.Fatalf("changes: %v", err)
	}
	if got := textsIn(res.Batches, validThID); len(got) != 1 || got[0] != "after-nometa" {
		t.Fatalf("nil-metadata thread aborted archived listing: batches=%+v", res.Batches)
	}
	cur, ok := readCursor(res.Next)
	if !ok {
		t.Fatalf("bad cursor: %s", res.Next)
	}
	if lp, exists := cur.List[forum.String()]; exists && lp.Upto <= 0 {
		t.Fatalf("zero archivedAt corrupted cursor List.Upto to %d", lp.Upto)
	}
}

func TestHardeningRateLimitBoundsAndSnowflakeAndComponentTextAndChannelsCap(t *testing.T) {
	// 1. NaN / Inf / huge X-RateLimit-Reset-After and Retry-After must not overflow or lock out past 1h.
	a := newAPI("https://discord.example", "tok", "0.1.2", &http.Client{})
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset-After", "NaN")
	a.note("/bad-nan", h)
	if w, _ := a.wait("/bad-nan"); w > 0 {
		t.Fatalf("NaN reset-after produced positive wait %v", w)
	}
	h.Set("X-RateLimit-Reset-After", "1e20")
	a.note("/huge", h)
	if w, _ := a.wait("/huge"); w <= 0 || w > time.Hour+5*time.Second {
		t.Fatalf("huge reset-after wait = %v, want bounded (0, 1h]", w)
	}
	e := rateLimited(http.Header{"Retry-After": []string{"1e20"}}, nil)
	if e.RetryAfter <= 0 || e.RetryAfter > time.Hour {
		t.Fatalf("huge Retry-After = %v, want bounded (0, 1h]", e.RetryAfter)
	}
	eNaN := rateLimited(http.Header{"Retry-After": []string{"NaN"}}, nil)
	if eNaN.RetryAfter != time.Second {
		t.Fatalf("NaN Retry-After = %v, want 1s", eNaN.RetryAfter)
	}

	// 2. snowflake.UnmarshalJSON rejects double-quoted garbage ""123"" and accepts whitespace-padded "123".
	var s snowflake
	if err := json.Unmarshal([]byte(`"123"`), &s); err != nil || s != 123 {
		t.Fatalf("valid snowflake: %d %v", s, err)
	}
	if err := (&s).UnmarshalJSON([]byte(` ""123"" `)); err == nil {
		t.Fatalf("double-quoted snowflake should fail, got %d", s)
	}
	if err := (&s).UnmarshalJSON([]byte(` "456" `)); err != nil || s != 456 {
		t.Fatalf("whitespace-padded snowflake failed: %d %v", s, err)
	}

	// 3. componentText must be deterministic across repeated runs and survive deep nesting.
	comp := json.RawMessage(`{"a":{"type":10,"content":"alpha"},"z":{"type":10,"content":"zeta"},"m":{"type":10,"content":"mid"}}`)
	want := componentText([]json.RawMessage{comp})
	for range 30 {
		if got := componentText([]json.RawMessage{comp}); got != want {
			t.Fatalf("componentText order is non-deterministic: %q vs %q", got, want)
		}
	}

	// 4. channelsTool 300-channel cap must break out of outer guilds loop so truncation notice appears once.
	f := newFakeDiscord(t)
	g1 := f.addGuild("Server One")
	for i := range 305 {
		f.addChannel(g1, fmt.Sprintf("ch-%d", i), chText)
	}
	g2 := f.addGuild("Server Two")
	f.addChannel(g2, "extra", chText)
	r := start(t, f)
	r.signIn()
	out, err := r.tool("discord_channels", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "more channels left out"); n != 1 {
		t.Fatalf("expected 1 truncation line, got %d", n)
	}

	// 5. Re-initializing conn must not panic on close(c.ready).
	in := &ext.Init{}
	if _, err := r.c.init(context.Background(), in, r.c.hub); err != nil {
		t.Fatalf("second init failed: %v", err)
	}
}
