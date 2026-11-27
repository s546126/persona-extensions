package github

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const messageLastPrefix = "gh-message:"

type messageHead struct {
	At  time.Time `json:"at"`
	IDs []string  `json:"ids"`
}

type inboxMessageRecord struct {
	Last string    `json:"last"`
	Seen time.Time `json:"seen"`
}

// Keep the IDs at the newest message time, including deleted ones. IDs belong to their API's namespace;
// neither same-second messages nor deleting the newest message can be represented by a timestamp alone.
func newestMessageLast(messages []move, previous string) (last, newestID string) {
	var head messageHead
	if strings.HasPrefix(previous, messageLastPrefix) {
		_ = json.Unmarshal([]byte(strings.TrimPrefix(previous, messageLastPrefix)), &head)
	}
	known := make(map[string]bool, len(head.IDs))
	for _, id := range head.IDs {
		known[id] = true
	}
	for _, message := range messages {
		if message.at.IsZero() || message.at.Before(head.At) {
			continue
		}
		if message.at.After(head.At) {
			head.At, known = message.at, map[string]bool{}
		}
		if !known[message.id] {
			newestID = message.id
		}
		known[message.id] = true
	}
	if head.At.IsZero() {
		return "", ""
	}
	head.IDs = make([]string, 0, len(known))
	for id := range known {
		head.IDs = append(head.IDs, id)
	}
	sort.Strings(head.IDs)
	raw, _ := json.Marshal(head)
	return messageLastPrefix + string(raw), newestID
}

func messageRecordKey(thread, account string) string {
	return strings.ToLower(account) + "/" + thread
}

func (c *Connector) previousMessageLast(ctx context.Context, thread, account string) string {
	key := messageRecordKey(thread, account)
	c.inbox.mu.Lock()
	record, ok := c.inbox.messages[key]
	c.inbox.mu.Unlock()
	if ok {
		return record.Last
	}
	return c.deviceState(ctx).InboxMessages[key].Last
}

// Save before publishing or consuming the notification. A failed store write must be retried, not silently
// acknowledged. The existing sealed login record survives extension restarts; each account has its own cursor.
func (c *Connector) rememberMessageLast(ctx context.Context, thread, account, last string) error {
	if !strings.HasPrefix(last, messageLastPrefix) {
		return nil
	}
	key, now := messageRecordKey(thread, account), time.Now().UTC()
	c.inbox.mu.Lock()
	record := c.inbox.messages[key]
	c.inbox.mu.Unlock()
	if record.Last == last && now.Sub(record.Seen) < 24*time.Hour {
		return nil
	}
	c.deviceState(ctx)
	c.mu.Lock()
	none, loaded := c.secrets == nil, c.devLoaded
	c.mu.Unlock()
	record = inboxMessageRecord{Last: last, Seen: now}
	if !none {
		if !loaded {
			return errors.New("the GitHub message cursor store could not be read")
		}
		if err := c.saveDevice(ctx, func(d *deviceLogin) {
			if d.InboxMessages == nil {
				d.InboxMessages = map[string]inboxMessageRecord{}
			}
			d.InboxMessages[key] = record
			for key, old := range d.InboxMessages {
				if now.Sub(old.Seen) > forgetAfter {
					delete(d.InboxMessages, key)
				}
			}
		}); err != nil {
			return err
		}
	}
	c.inbox.mu.Lock()
	defer c.inbox.mu.Unlock()
	if c.inbox.messages == nil {
		c.inbox.messages = map[string]inboxMessageRecord{}
	}
	c.inbox.messages[key] = record
	for key, old := range c.inbox.messages {
		if now.Sub(old.Seen) > forgetAfter {
			delete(c.inbox.messages, key)
		}
	}
	return nil
}
