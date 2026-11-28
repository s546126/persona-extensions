package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

type notice struct {
	Title   string              `json:"title"`
	Body    string              `json:"body"`
	ID      string              `json:"id"`
	Level   string              `json:"level"`
	UI      string              `json:"ui"`
	State   string              `json:"state"`
	Actions []wire.NotifyAction `json:"actions"`
}

func notification(c config, s state, now time.Time) notice {
	n := total(s, now, s.location())
	body := "今天还没喝水，先来一杯吧"
	if n > 0 {
		body = fmt.Sprintf("今天已喝 %d / %d 毫升，还差约 %d 杯", n, c.Goal, cups(c, n))
	}
	return notice{Title: "该喝水了", Body: body, ID: "water-reminder", Level: "active", UI: "water",
		State: fmt.Sprintf("r=%d", now.UnixMilli()), Actions: []wire.NotifyAction{
			{ID: "drink", Title: "喝了一杯"}, {ID: "snooze", Title: "等会儿"},
			{ID: "log", Title: "记别的量", Input: &wire.NotifyActionInput{Placeholder: "毫升，比如 300"}},
		}}
}

func cups(c config, n int) int { return (max(c.Goal-n, 0) + c.Cup - 1) / c.Cup }

type delivery struct {
	Sent   bool   `json:"sent"`
	Reason string `json:"reason"`
}
type hubClient struct {
	base, token string
	scopes      []string
	client      *http.Client
	// lnMu guards the low-distraction cache only. It is never held across a request, so a slow hub
	// read cannot block the scheduler or page actions, which need app.mu instead.
	lnMu sync.Mutex
	lnAt time.Time
	ln   *bool
}

func newHub(base, token string, scopes []string) *hubClient {
	return &hubClient{base: strings.TrimRight(base, "/"), token: token, scopes: slices.Clone(scopes),
		client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (h *hubClient) can() bool {
	return h != nil && h.base != "" && h.token != "" && slices.Contains(h.scopes, "notify")
}

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("提醒没有送达（%d）", int(e)) }
func retryable(err error) bool      { var s statusError; return !errors.As(err, &s) || s >= 500 }

// Reasons kept in state. Hub reason codes are stored as the hub sent them; the constants below are
// local outcomes. Only deliveryNote (ui.go) turns a stored reason into page text.
const (
	reasonUnknown     = "unknown"     // the hub said not sent, with no usable reason
	reasonUnreachable = "unreachable" // no answer, or a 5xx, and no attempt is left
	reasonRejected    = "rejected"    // the hub answered 4xx; that is not retried
	reasonUnconfirmed = "unconfirmed" // an earlier attempt's outcome was lost, so retries stopped
)

// failureReason classifies a request that failed without a hub decision.
func failureReason(err error) string {
	var s statusError
	if errors.As(err, &s) && int(s) < 500 {
		return reasonRejected
	}
	return reasonUnreachable
}

func (h *hubClient) call(ctx context.Context, method, path string, in, out any) error {
	if !h.can() {
		return statusError(http.StatusForbidden)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return errors.New("暂时连不上提醒服务")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(resp.StatusCode)
	}
	const maxBody = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(raw) > maxBody {
		return errors.New("提醒服务返回内容过大 (response too large)")
	}
	return json.Unmarshal(raw, out)
}
func (h *hubClient) notify(ctx context.Context, n notice) (delivery, error) {
	var d delivery
	err := h.call(ctx, http.MethodPost, "/api/v1/notify", n, &d)
	return d, err
}

// lowDistraction returns the cached ln-mode value and refreshes it after three minutes. Callers must not
// hold app.mu. A caller that arrives during a refresh sees unknown at once instead of waiting for it.
func (h *hubClient) lowDistraction(ctx context.Context, now time.Time) *bool {
	if !h.can() {
		return nil
	}
	h.lnMu.Lock()
	if !h.lnAt.IsZero() && now.Sub(h.lnAt) < 3*time.Minute {
		ln := h.ln
		h.lnMu.Unlock()
		return ln
	}
	h.lnAt = now
	h.ln = nil
	h.lnMu.Unlock()
	var out struct {
		Enabled *bool `json:"enabled"`
	}
	var ln *bool
	if h.call(ctx, http.MethodGet, "/api/v1/push/ln-mode", nil, &out) == nil {
		ln = out.Enabled
	}
	h.lnMu.Lock()
	defer h.lnMu.Unlock()
	if h.lnAt.Equal(now) {
		h.ln = ln
	}
	return ln
}

// due reports whether the next reminder is at or before now. It does not look at the retry gap.
func (a *app) due(now time.Time) bool {
	p := schedule(a.cfg, a.s, now)
	return !p.Next.IsZero() && !p.Next.After(now)
}

// tick runs one scheduling pass. The caller holds app.mu and passes the clock it read while holding it.
// It reserves and persists an attempt before sending, so a crash cannot repeat it, and checks the window
// once more after that save and before the request goes out. There are at most three attempts, thirty
// seconds apart, including across restarts. Logging cancels the outstanding occurrence.
func (a *app) tick(ctx context.Context, now time.Time) error {
	a.refreshWidget(now)
	pruned := a.s.clone()
	pruned.prune(now)
	if len(pruned.Records) != len(a.s.Records) || len(pruned.Seen) != len(a.s.Seen) {
		if err := a.update(now, func(*state) {}); err != nil {
			return err
		}
	}
	if !a.hub.can() {
		return nil
	}
	if a.s.Pending != nil && day(a.s.Pending.At, a.s.location()) != day(now, a.s.location()) {
		if err := a.update(now, func(s *state) { s.Pending = nil }); err != nil {
			return err
		}
	}
	if !a.due(now) {
		return nil
	}
	if a.s.Pending != nil && now.Before(a.s.Pending.RetryAt) {
		return nil
	}
	if a.s.Pending != nil && a.s.Pending.Attempts >= 3 {
		return a.update(now, func(s *state) {
			s.LastReminder = now
			s.Pending = nil
			s.Snooze = time.Time{}
			s.Reason = reasonUnconfirmed
		})
	}
	prev := a.s.Pending
	if err := a.update(now, func(s *state) {
		if s.Pending == nil {
			s.Pending = &pending{At: now, Notice: notification(a.cfg, *s, now)}
		}
		s.Pending.Attempts++
		s.Pending.RetryAt = now.Add(30 * time.Second)
	}); err != nil {
		return err
	}
	// The clock may have moved past the window while the reservation was being saved.
	if fresh := a.now(); !a.due(fresh) {
		return a.update(fresh, func(s *state) { s.Pending = prev })
	}
	d, err := a.hub.notify(ctx, a.s.Pending.Notice)
	if err != nil {
		log.Printf("喝水提醒发送失败：%v", err)
	} else if !d.Sent {
		log.Printf("喝水提醒没有送达：%s", d.Reason)
	}
	return a.update(now, func(s *state) {
		if err != nil && retryable(err) && s.Pending.Attempts < 3 {
			return
		}
		s.LastReminder = now
		s.Snooze = time.Time{}
		s.Pending = nil
		s.Reason = ""
		if err != nil {
			s.Reason = failureReason(err)
		} else if !d.Sent {
			s.Reason = d.Reason
			if s.Reason == "" {
				s.Reason = reasonUnknown
			}
			// Keep external reason strings within the UI text limit.
			r := []rune(s.Reason)
			if len(r) > 100 {
				s.Reason = string(r[:100])
			}
		}
	})
}
