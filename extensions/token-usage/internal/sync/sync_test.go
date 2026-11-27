package sync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/budget"
	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/hub"
	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/store"
)

// served records what the fake hub was asked and the notifications it got.
type served struct {
	mu     sync.Mutex
	ranges []string
	notes  []string
}

func (s *served) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/machines":
		write(w, map[string]any{"machines": []hub.Machine{{ID: "m-a", Name: "电脑 A", Online: true}}})
	case strings.HasPrefix(r.URL.Path, "/api/v1/machines/"):
		s.mu.Lock()
		s.ranges = append(s.ranges, r.URL.Query().Get("range")+" "+r.URL.Query().Get("model"))
		s.mu.Unlock()
		if r.URL.Query().Get("model") == "" {
			write(w, map[string]any{"usage": map[string]any{
				"models": []hub.Share{{Name: "alpha", In: 100}}, "launchers": []hub.Share{{Name: "cli"}},
			}})
			return
		}
		write(w, map[string]any{"usage": hub.Series{Step: 60, Points: []hub.Point{{T: time.Now().Unix() / 60 * 60, In: 100}}}})
	case r.URL.Path == "/api/v1/notify":
		var body struct{ Title string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.notes = append(s.notes, body.Title)
		s.mu.Unlock()
		write(w, map[string]any{"sent": true})
	default:
		http.NotFound(w, r)
	}
}

func write(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func TestLoopBackfillsThenPollsAndAlerts(t *testing.T) {
	var s served
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()

	st := store.Open("")
	bf := budget.Open("")
	if err := bf.Set(budget.Budget{Span: budget.Daily, Limit: 50}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		Loop(ctx, hub.New(srv.URL, "tok", nil), st, bf, time.UTC, 20*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.ranges)
		s.mu.Unlock()
		if n >= 4 { // two pulls, each an unfiltered read plus one filtered read
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ranges) < 2 || !strings.HasPrefix(s.ranges[0], "7d") {
		t.Fatalf("ranges %v, the first pull should ask for 7d", s.ranges)
	}
	polled := false
	for _, r := range s.ranges[2:] {
		polled = polled || strings.HasPrefix(r, "24h")
	}
	if !polled {
		t.Fatalf("no 24h pull after the backfill: %v", s.ranges)
	}
	var total int64
	_ = st.View(func(st *store.State) error {
		for _, d := range st.Days {
			for _, b := range d.Buckets {
				total += b.Total()
			}
		}
		return nil
	})
	if total == 0 {
		t.Fatal("nothing was stored")
	}
	if len(s.notes) == 0 {
		t.Fatal("the budget was crossed but no notification was sent")
	}
}

func TestPullAppliesUpstreamZeroCorrection(t *testing.T) {
	minute := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC).Unix()
	var mu sync.Mutex
	point := hub.Point{T: minute, In: 100, Req: 3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/machines":
			write(w, map[string]any{"machines": []hub.Machine{{ID: "m-a"}}})
		case "/api/v1/machines/m-a/usage":
			if r.URL.Query().Get("model") == "" {
				write(w, map[string]any{"usage": map[string]any{
					"models": []hub.Share{{Name: "alpha"}}, "launchers": []hub.Share{{Name: "cli"}},
				}})
				return
			}
			mu.Lock()
			p := point
			mu.Unlock()
			write(w, map[string]any{"usage": hub.Series{Step: 60, Points: []hub.Point{p}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	st := store.Open("")
	bf := budget.Open("")
	client := hub.New(srv.URL, "tok", nil)
	readTotals := func() (int64, int) {
		var total int64
		var requests int
		if err := st.View(func(state *store.State) error {
			for _, day := range state.Days {
				for _, b := range day.Buckets {
					total += b.Total()
					requests += b.Requests
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return total, requests
	}

	backfilled := map[string]bool{"m-a": true}
	pull(context.Background(), client, st, bf, time.UTC, backfilled)
	if total, requests := readTotals(); total != 100 || requests != 3 {
		t.Fatalf("first pull stored total %d and requests %d, want 100 and 3", total, requests)
	}

	mu.Lock()
	point = hub.Point{T: minute}
	mu.Unlock()
	pull(context.Background(), client, st, bf, time.UTC, backfilled)
	if total, requests := readTotals(); total != 0 || requests != 0 {
		t.Fatalf("zero correction left total %d and requests %d, want zero", total, requests)
	}
}

// A hub without Postgres holds only the last 26 hours, so its 7d range reads zero before that: those zeros must not
// delete the history the 24h pulls stored. A hub that stores the week is trusted for the whole range.
func TestBackfillZerosDeleteOnlyWhenTheHubStoresTheWeek(t *testing.T) {
	minute := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC).Unix()
	var mu sync.Mutex
	point, stored := hub.Point{T: minute, In: 100, Req: 3}, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/machines":
			write(w, map[string]any{"machines": []hub.Machine{{ID: "m-a"}}})
		case "/api/v1/machines/m-a/usage":
			if r.URL.Query().Get("model") == "" {
				write(w, map[string]any{"usage": map[string]any{
					"models": []hub.Share{{Name: "alpha"}}, "launchers": []hub.Share{{Name: "cli"}},
				}})
				return
			}
			mu.Lock()
			s := hub.Series{Step: 60, Points: []hub.Point{point}, Stored: stored}
			mu.Unlock()
			write(w, map[string]any{"usage": s})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	st := store.Open("")
	bf := budget.Open("")
	client := hub.New(srv.URL, "tok", nil)
	total := func() int64 {
		var n int64
		if err := st.View(func(state *store.State) error {
			for _, day := range state.Days {
				for _, b := range day.Buckets {
					n += b.Total()
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}

	pull(context.Background(), client, st, bf, time.UTC, map[string]bool{"m-a": true})
	mu.Lock()
	point = hub.Point{T: minute}
	mu.Unlock()
	pull(context.Background(), client, st, bf, time.UTC, map[string]bool{})
	if n := total(); n != 100 {
		t.Fatalf("a 7d zero from a hub that keeps only memory left total %d, want 100 kept", n)
	}
	mu.Lock()
	stored = true
	mu.Unlock()
	pull(context.Background(), client, st, bf, time.UTC, map[string]bool{})
	if n := total(); n != 0 {
		t.Fatalf("a 7d zero from a hub that stores the week left total %d, want 0", n)
	}
}

func TestLoopRetriesInitialBackfillAfterRosterOutage(t *testing.T) {
	var rosterRequested atomic.Bool
	ranges := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/machines":
			if !rosterRequested.Swap(true) {
				http.Error(w, "temporary roster outage", http.StatusServiceUnavailable)
				return
			}
			write(w, map[string]any{"machines": []hub.Machine{{ID: "sample", Online: true}}})
		case "/api/v1/machines/sample/usage":
			rng := r.URL.Query().Get("range")
			select {
			case ranges <- rng:
			default:
			}
			write(w, map[string]any{"usage": hub.Series{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	st, bf := store.Open(t.TempDir()), budget.Open(t.TempDir())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Loop(ctx, hub.New(server.URL, "fixture", nil), st, bf, time.UTC, 20*time.Millisecond)
	}()
	defer func() {
		cancel()
		<-done
	}()

	var got []string
	for len(got) < 2 {
		select {
		case rng := <-ranges:
			got = append(got, rng)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for recovery and follow-up usage requests; got %v", got)
		}
	}
	if got[0] != "7d" {
		t.Fatalf("first usage request after recovery used %q; want 7d to recover the available history", got[0])
	}
	if got[1] != "24h" {
		t.Fatalf("second usage request after backfill used %q; want 24h", got[1])
	}
}

func TestPullBackfillsPerMachineIndependently(t *testing.T) {
	minute := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC).Unix()
	var mu sync.Mutex
	round := 0
	var requests []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cur := round
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/machines":
			machines := []hub.Machine{{ID: "m-a", Online: true}, {ID: "m-b", Online: true}}
			if cur >= 3 {
				machines = append(machines, hub.Machine{ID: "m-c", Online: true})
			}
			write(w, map[string]any{"machines": machines})
		case "/api/v1/machines/m-a/usage", "/api/v1/machines/m-b/usage", "/api/v1/machines/m-c/usage":
			parts := strings.Split(r.URL.Path, "/")
			id := parts[4]
			rng := r.URL.Query().Get("range")
			model := r.URL.Query().Get("model")
			if model == "" {
				mu.Lock()
				requests = append(requests, id+":"+rng)
				mu.Unlock()
			}
			if id == "m-b" && cur == 1 {
				http.Error(w, "temporary usage outage", http.StatusServiceUnavailable)
				return
			}
			if id == "m-b" && cur == 2 && model != "" {
				http.Error(w, "partial usage outage", http.StatusServiceUnavailable)
				return
			}
			if model == "" {
				write(w, map[string]any{"usage": map[string]any{
					"models": []hub.Share{{Name: "alpha"}}, "launchers": []hub.Share{{Name: "cli"}},
				}})
				return
			}
			tokens := map[string]int64{"m-a": 100, "m-b": 200, "m-c": 300}[id]
			write(w, map[string]any{"usage": hub.Series{Step: 60, Points: []hub.Point{{T: minute, In: tokens, Req: 1}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	st := store.Open("")
	bf := budget.Open("")
	client := hub.New(srv.URL, "tok", nil)
	backfilled := map[string]bool{}

	runRound := func(n int) []string {
		mu.Lock()
		round = n
		requests = nil
		mu.Unlock()
		pull(context.Background(), client, st, bf, time.UTC, backfilled)
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}

	// Round 1: m-a succeeds (7d), m-b fails shares with 503.
	if got, want := runRound(1), []string{"m-a:7d", "m-b:7d"}; !slices.Equal(got, want) {
		t.Fatalf("round 1 requests %v, want %v", got, want)
	}
	// Round 2: m-a advances to 24h; m-b retries 7d and fails filtered series with 503.
	if got, want := runRound(2), []string{"m-a:24h", "m-b:7d"}; !slices.Equal(got, want) {
		t.Fatalf("round 2 requests %v, want %v", got, want)
	}
	// Round 3: m-a stays on 24h; m-b retries 7d and succeeds; late-joining m-c gets 7d.
	if got, want := runRound(3), []string{"m-a:24h", "m-b:7d", "m-c:7d"}; !slices.Equal(got, want) {
		t.Fatalf("round 3 requests %v, want %v", got, want)
	}
	// Round 4: all three machines have completed backfill and poll 24h.
	if got, want := runRound(4), []string{"m-a:24h", "m-b:24h", "m-c:24h"}; !slices.Equal(got, want) {
		t.Fatalf("round 4 requests %v, want %v", got, want)
	}

	totals := map[string]int64{}
	if err := st.View(func(state *store.State) error {
		for _, day := range state.Days {
			for _, b := range day.Buckets {
				totals[day.Machine] += b.Total()
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if totals["m-a"] != 100 || totals["m-b"] != 200 || totals["m-c"] != 300 {
		t.Fatalf("stored totals %v, want m-a=100 m-b=200 m-c=300", totals)
	}
}

func TestPullRetriesBackfillAfterEmptyRoster(t *testing.T) {
	var mu sync.Mutex
	showMachine := false
	var ranges []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/machines":
			mu.Lock()
			ok := showMachine
			mu.Unlock()
			var machines []hub.Machine
			if ok {
				machines = []hub.Machine{{ID: "m-a", Online: true}}
			}
			write(w, map[string]any{"machines": machines})
		case "/api/v1/machines/m-a/usage":
			mu.Lock()
			ranges = append(ranges, r.URL.Query().Get("range"))
			mu.Unlock()
			write(w, map[string]any{"usage": hub.Series{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	st := store.Open("")
	bf := budget.Open("")
	client := hub.New(srv.URL, "tok", nil)
	backfilled := map[string]bool{}

	pull(context.Background(), client, st, bf, time.UTC, backfilled)
	mu.Lock()
	showMachine = true
	mu.Unlock()
	pull(context.Background(), client, st, bf, time.UTC, backfilled)
	pull(context.Background(), client, st, bf, time.UTC, backfilled)

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"7d", "24h"}; !slices.Equal(ranges, want) {
		t.Fatalf("ranges %v, want %v", ranges, want)
	}
}

func TestPullRetriesBackfillWhenMergeFails(t *testing.T) {
	minute := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC).Unix()
	var mu sync.Mutex
	var ranges []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/machines":
			write(w, map[string]any{"machines": []hub.Machine{{ID: "m-a", Online: true}}})
		case "/api/v1/machines/m-a/usage":
			if r.URL.Query().Get("model") == "" {
				mu.Lock()
				ranges = append(ranges, r.URL.Query().Get("range"))
				mu.Unlock()
				write(w, map[string]any{"usage": map[string]any{
					"models": []hub.Share{{Name: "alpha"}}, "launchers": []hub.Share{{Name: "cli"}},
				}})
				return
			}
			write(w, map[string]any{"usage": hub.Series{Step: 60, Points: []hub.Point{{T: minute, In: 100, Req: 1}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	usageFile := filepath.Join(dir, "usage.json")
	if err := os.WriteFile(usageFile, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	st := store.Open(dir)
	bf := budget.Open(dir)
	client := hub.New(srv.URL, "tok", nil)
	backfilled := map[string]bool{}

	// Round 1: pull succeeds with 7d, but Merge refuses the corrupt file, so backfill is not marked complete.
	pull(context.Background(), client, st, bf, time.UTC, backfilled)
	if err := os.Remove(usageFile); err != nil {
		t.Fatal(err)
	}
	// Round 2: retries 7d and persists.
	pull(context.Background(), client, st, bf, time.UTC, backfilled)
	// Round 3: advances to 24h.
	pull(context.Background(), client, st, bf, time.UTC, backfilled)

	mu.Lock()
	gotRanges := slices.Clone(ranges)
	mu.Unlock()
	if want := []string{"7d", "7d", "24h"}; !slices.Equal(gotRanges, want) {
		t.Fatalf("ranges %v, want %v", gotRanges, want)
	}
	var total int64
	if err := st.View(func(state *store.State) error {
		for _, day := range state.Days {
			for _, b := range day.Buckets {
				total += b.Total()
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if total != 100 {
		t.Fatalf("stored total %d, want 100 after recovery", total)
	}
}

func TestLoopStopsWithoutAHub(t *testing.T) {
	done := make(chan struct{})
	go func() {
		Loop(context.Background(), hub.New("", "", nil), store.Open(""), budget.Open(""), time.UTC, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the loop ran without a hub address")
	}
}

// Keep io imported for the request body drain pattern used by other tests of this module.
var _ = io.Discard

// Each round reports how it ended to the client, so the page can tell the user why nothing new arrives: nothing read
// when the computer list fails, then nothing wrong once a round reads every computer.
func TestARoundReportsHowItEnded(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"machines": []hub.Machine{}})
	}))
	defer srv.Close()
	client := hub.New(srv.URL, "tok", nil)
	st, bf := store.Open(""), budget.Open("")

	if r := client.LastPull(); !r.At.IsZero() || r.Err != nil || len(r.Failed) > 0 {
		t.Fatalf("before any round: %+v", r)
	}
	pull(context.Background(), client, st, bf, time.UTC, map[string]bool{})
	r := client.LastPull()
	if r.At.IsZero() || r.Err == nil || !strings.Contains(r.Err.Error(), "自动再试") {
		t.Fatalf("after a failed round: %+v", r)
	}
	fail.Store(false)
	pull(context.Background(), client, st, bf, time.UTC, map[string]bool{})
	if r := client.LastPull(); r.Err != nil || len(r.Failed) > 0 {
		t.Fatalf("after a good round: %+v", r)
	}
}

// One computer that cannot be read is reported as that computer, by its display name, out of how many there are; the
// round itself did not fail and the others are still read.
func TestOneComputerFailingIsReportedByName(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/machines":
					_ = json.NewEncoder(w).Encode(map[string]any{"machines": []hub.Machine{
						{ID: "m-a", Name: "书房电脑"}, {ID: "m-b", Name: "旧笔记本"}, {ID: "m-c"},
					}})
				case strings.HasPrefix(r.URL.Path, "/api/v1/machines/m-b/"):
					http.Error(w, "no such machine", status)
				default:
					_ = json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{}})
				}
			}))
			defer srv.Close()
			client := hub.New(srv.URL, "tok", nil)
			backfilled := map[string]bool{}
			pull(context.Background(), client, store.Open(""), budget.Open(""), time.UTC, backfilled)
			r := client.LastPull()
			if r.Err != nil || r.Total != 3 || len(r.Failed) != 1 || r.Failed[0].Name != "旧笔记本" {
				t.Fatalf("report %+v", r)
			}
			if hub.NotFound(r.Failed[0].Err) != (status == http.StatusNotFound) {
				t.Fatalf("not found %v for %d", hub.NotFound(r.Failed[0].Err), status)
			}
			if !backfilled["m-a"] || !backfilled["m-c"] || backfilled["m-b"] {
				t.Fatalf("the other computers were not read: %v", backfilled)
			}
		})
	}
}
