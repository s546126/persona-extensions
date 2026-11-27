package main

// Marks enkeep_mark_wrong could not apply at once: a daily pass holds the store's lock for its whole run, tens of
// minutes, and the hub gives up on a tool call long before. The marks wait in marks-queue.json beside the state and
// are applied as soon as the lock frees (and at the next start, when the process stopped first).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const marksFile = "marks-queue.json"

// queuedMark is one enkeep_mark_wrong call waiting for the lock.
type queuedMark struct {
	IDs    []string `json:"ids"`
	Reason string   `json:"reason"`
	At     string   `json:"at"`
}

func (a *app) marksPath() string { return filepath.Join(a.c.St.Dir, marksFile) }

// readMarks is the queue; the caller holds a.marksMu.
func (a *app) readMarks() []queuedMark {
	raw, err := os.ReadFile(a.marksPath())
	if err != nil {
		return nil
	}
	var q []queuedMark
	if err := json.Unmarshal(raw, &q); err != nil {
		a.logf("enkeep-memory: %s: %v", marksFile, err)
		return nil
	}
	return q
}

// queueMarks keeps a mark_wrong call for when the running pass lets go of the state, and says so. Ids the last
// snapshot does not have are named at once; they are still queued, an entry may be new in the running pass.
func (a *app) queueMarks(ids []string, reason string) (string, error) {
	a.marksMu.Lock()
	q := append(a.readMarks(), queuedMark{IDs: ids, Reason: reason, At: time.Now().UTC().Format(time.RFC3339)})
	b, err := json.Marshal(q)
	if err == nil {
		err = os.WriteFile(a.marksPath(), b, 0o600)
	}
	a.marksMu.Unlock()
	if err != nil {
		return "", err
	}
	go a.applyMarks(context.Background())
	var unknown []string
	if snap := a.c.Live.Snapshot(); snap != nil && snap.State != nil {
		for _, id := range ids {
			if snap.State.Entries[id] == nil {
				unknown = append(unknown, id)
			}
		}
	}
	what := "a curation pass"
	if r := a.runningNow(); r != "" {
		what = r
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Queued: %s is running and holds the state, so %d entr(ies) will be marked wrong (by assistant, with the reason) as soon as it ends; they then leave every exit.", what, len(ids))
	if len(unknown) > 0 {
		fmt.Fprintf(&out, " Not in the last snapshot (skipped then if still unknown): %s.", strings.Join(unknown, ", "))
	}
	return out.String(), nil
}

// applyMarks applies the queued marks, waiting for the store's lock as long as a pass holds it, then compiles the
// exits without the entries, saves and trims only the applied prefix from the queue.
// Lock order: never hold a.marksMu while waiting for a.c.St.Lock(), so concurrent enkeep_mark_wrong calls never block.
func (a *app) applyMarks(ctx context.Context) {
	a.marksMu.Lock()
	hasPending := len(a.readMarks()) > 0
	a.marksMu.Unlock()
	if !hasPending {
		return
	}
	a.c.St.Lock()
	defer a.c.St.Unlock()
	for {
		a.marksMu.Lock()
		q := a.readMarks()
		a.marksMu.Unlock()
		if len(q) == 0 {
			return
		}
		var marked, missing []string
		for _, m := range q {
			for _, id := range m.IDs {
				if _, err := a.c.MarkWrongBy(id, "assistant", m.Reason); err != nil {
					missing = append(missing, id)
					continue
				}
				marked = append(marked, id)
			}
		}
		if len(marked) > 0 {
			a.withdraw(ctx)
			if err := a.c.St.Save(); err != nil {
				a.logf("enkeep-memory: queued marks: %v", err) // the queue stays: tried again at the next start
				return
			}
			a.c.Publish()
		}
		// Trim only the applied prefix so marks appended concurrently during withdraw/Save are preserved and applied next.
		a.marksMu.Lock()
		cur := a.readMarks()
		n := min(len(q), len(cur))
		rest := cur[n:]
		var werr error
		if len(rest) == 0 {
			if err := os.Remove(a.marksPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				werr = err
			}
		} else {
			b, err := json.Marshal(rest)
			if err == nil {
				err = os.WriteFile(a.marksPath(), b, 0o600)
			}
			werr = err
		}
		a.marksMu.Unlock()
		if werr != nil {
			a.logf("enkeep-memory: %s: %v", marksFile, werr)
		}
		slices.Sort(missing)
		a.logf("enkeep-memory: queued marks applied: %d marked wrong, not found: %v", len(marked), slices.Compact(missing))
		if a.hub != nil {
			_ = a.hub.Invalidate("enkeep-memory")
			_ = a.hub.Invalidate("enkeep-memory-today")
		}
		if werr != nil || len(rest) == 0 {
			return
		}
	}
}
