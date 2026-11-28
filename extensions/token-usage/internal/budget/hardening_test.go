package budget

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/store"
)

func TestHardeningConcurrentAccessRace(t *testing.T) {
	f := Open("")
	st := &store.State{Days: []store.Day{{
		Date:    "2026-11-13",
		Machine: "m1",
		Buckets: []store.Bucket{{Minute: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC).Unix(), Model: "alpha", Input: 900}},
	}}}
	now := time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				switch id % 3 {
				case 0:
					_ = f.Set(Budget{Span: Daily, Model: "alpha", Limit: int64(1000 + j)})
				case 1:
					_, _ = f.Statuses(st, now, time.UTC)
				case 2:
					_, _ = f.Alerts(st, now, time.UTC)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestHardeningNonPositiveLimitNoNaNInfAndMinInt64Comma(t *testing.T) {
	dir := t.TempDir()
	bad := State{Format: format, Budgets: []Budget{{Span: Daily, Model: "alpha", Limit: 0}, {Span: Weekly, Model: "beta", Limit: -5}}}
	raw, _ := json.Marshal(bad)
	if err := os.WriteFile(filepath.Join(dir, fileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f := Open(dir)
	st := &store.State{Days: []store.Day{{
		Date:    "2026-11-13",
		Machine: "m1",
		Buckets: []store.Bucket{{Minute: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC).Unix(), Model: "alpha", Input: 100}},
	}}}
	now := time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)

	statuses, err := f.Statuses(st, now, time.UTC)
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	for _, s := range statuses {
		if math.IsNaN(s.Ratio) || math.IsInf(s.Ratio, 0) {
			t.Fatalf("non-finite ratio for budget %+v: %v", s.Budget, s.Ratio)
		}
	}
	if _, err := json.Marshal(statuses); err != nil {
		t.Fatalf("json.Marshal(statuses) failed: %v", err)
	}
	if _, err := f.Alerts(st, now, time.UTC); err != nil {
		t.Fatalf("Alerts: %v", err)
	}

	// MinInt64 must not infinitely recurse.
	if got := comma(math.MinInt64); got != "-9,223,372,036,854,775,808" {
		t.Fatalf("comma(MinInt64) = %q", got)
	}
}
