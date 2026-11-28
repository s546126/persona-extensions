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
	"time"
)

type notice struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	ID    string `json:"id"`
	Level string `json:"level"`
	UI    string `json:"ui"`
	State string `json:"state,omitempty"`
}

// hubNotifier sends a notification through the hub (POST /api/v1/notify, scope notify).
func hubNotifier(base, token string, scopes []string) func(context.Context, notice) error {
	base = strings.TrimRight(base, "/")
	if base == "" || token == "" || !slices.Contains(scopes, "notify") {
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context, n notice) error {
		body, err := json.Marshal(n)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/notify", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if res.StatusCode/100 != 2 {
			return fmt.Errorf("Persona 没收下这条提醒（%d）", res.StatusCode)
		}
		var out struct {
			Sent   bool   `json:"sent"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(data, &out) == nil && !out.Sent && out.Reason != "" {
			log.Printf("学习：提醒没发出：%s", out.Reason)
		}
		return nil
	}
}

func (a *app) run(ctx context.Context) {
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.tick(ctx); err != nil && ctx.Err() == nil {
				log.Printf("学习：%v", err)
			}
			t.Reset(time.Minute)
		}
	}
}

// tick sends the day's reminder once, at the set time or the first tick after it, when cards are due. A day the
// program was not running at the time gets no late reminder after midnight.
func (a *app) tick(ctx context.Context) error {
	a.mu.Lock()
	now := a.now()
	local := now.In(a.loc)
	today := local.Format(time.DateOnly)
	if !a.cfg.Remind || a.notify == nil || a.st.LastRemind == today || local.Format("15:04") < a.cfg.RemindAt {
		a.mu.Unlock()
		return nil
	}
	due := len(queue(a.st, now, a.loc, a.cfg.NewPerDay))
	err := a.update(func(s *state) error { s.LastRemind = today; return nil })
	send := a.notify
	a.mu.Unlock()
	if err != nil || due == 0 {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := send(ctx, notice{Title: "该复习了", Body: fmt.Sprintf("今天有 %d 张卡片要复习，几分钟就好", due),
		ID: "study-daily", Level: "active", UI: pageID}); err != nil {
		return errors.Join(errors.New("每日提醒没发出"), err)
	}
	return nil
}
