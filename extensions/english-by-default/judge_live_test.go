package main

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// TestJudgeLive measures the judge on testdata/judge.jsonl and judge-holdout.jsonl with the real model. It costs model calls, so it runs
// only when EBD_LIVE_JUDGE is set (to the number of rounds): EBD_LIVE_JUDGE=1 go test -run TestJudgeLive -v.
func TestJudgeLive(t *testing.T) {
	rounds := 0
	if v := os.Getenv("EBD_LIVE_JUDGE"); v == "" {
		t.Skip("EBD_LIVE_JUDGE is not set")
	} else if err := json.Unmarshal([]byte(v), &rounds); err != nil || rounds < 1 {
		rounds = 1
	}
	var b []byte
	for _, name := range []string{"testdata/judge.jsonl", "testdata/judge-holdout.jsonl"} {
		f, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		b = append(append(b, f...), '\n')
	}
	type jcase struct {
		Want              bool
		Original, Attempt string
	}
	var cs []jcase
	for _, line := range splitLines(string(b)) {
		var c jcase
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	ask := commandLLM(defaultOptions.LLMCommand)
	type result struct {
		I, Round int
		V        verdict
		Err      string
	}
	var mu sync.Mutex
	var out []result
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for r := range rounds {
		for i, c := range cs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()
				v, err := judge(ctx, ask, c.Original, c.Attempt)
				res := result{I: i, Round: r, V: v}
				if err != nil {
					res.Err = err.Error()
				}
				mu.Lock()
				out = append(out, res)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	ok, errs := 0, 0
	for _, r := range out {
		c := cs[r.I]
		got := passed(r.V, defaultOptions.JudgeThreshold)
		if r.Err != "" {
			errs++
			t.Logf("error on %q / %q: %s", c.Original, c.Attempt, r.Err)
			continue
		}
		if got == c.Want {
			ok++
		} else {
			t.Logf("wrong (want %v): %q / %q → equivalent %v score %d hint %q", c.Want, c.Original, c.Attempt, r.V.Equivalent, r.V.Score, r.V.Hint)
		}
		j, _ := json.Marshal(map[string]any{"i": r.I, "want": c.Want, "eq": r.V.Equivalent, "score": r.V.Score})
		t.Logf("raw %s", j)
	}
	t.Logf("judge accuracy %d/%d, errors %d", ok, len(out), errs)
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if line := s[start:i]; len(line) > 0 {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}
