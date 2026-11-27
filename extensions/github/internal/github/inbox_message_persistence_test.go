package github

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDeletedNewestMessageStaysConsumedAfterRestart(t *testing.T) {
	f, c := newestMessageFixture(t)
	store := &memSecrets{}
	c.UseSecrets(store)
	at := fixtureAt(9 * time.Hour)
	later := fixtureAt(9*time.Hour + time.Minute)
	comment := `{"id":101,"user":{"login":"alex"},"body":"Earlier synthetic comment.","created_at":"` + at + `"}`
	replaceNewestMessageData(t, f, c, func() {
		f.comments["/repos/example-org/example-repo/issues/12/comments"] = `[` + comment +
			`,{"id":102,"user":{"login":"blair"},"body":"Deleted synthetic comment.","created_at":"` + later + `"}]`
		f.notifications = "[" + note("1", "author", "PullRequest", "https://api.github.com"+api12, t1) + "]"
	})
	before := things(t, c)[newestMessageThread]
	if before.Last == "" || before.Preview != "Deleted synthetic comment." {
		t.Fatalf("initial newest message was not observed: %+v", before)
	}
	restarted := f.server(t)
	restarted.UseSecrets(store)
	restarted.check(context.Background())
	replaceNewestMessageData(t, f, restarted, func() {
		f.comments["/repos/example-org/example-repo/issues/12/comments"] = `[` + comment + `]`
		f.notifications = "[" + note("1", "author", "PullRequest", "https://api.github.com"+api12, t2) + "]"
	})
	if after := things(t, restarted)[newestMessageThread]; after.Last != before.Last {
		t.Fatalf("restart replayed a deleted message: %q -> %q", before.Last, after.Last)
	}
}

type messageSaveFailure struct {
	memSecrets
	fail bool
}

func (s *messageSaveFailure) Save(ctx context.Context, raw []byte) error {
	if s.fail {
		return errors.New("synthetic cursor save failure")
	}
	return s.memSecrets.Save(ctx, raw)
}

func TestMessageCursorSaveFailureRetriesBeforePublishing(t *testing.T) {
	f, c := newestMessageFixture(t)
	store := &messageSaveFailure{fail: true}
	c.UseSecrets(store)
	replaceNewestMessageData(t, f, c, func() {
		f.comments["/repos/example-org/example-repo/issues/12/comments"] =
			`[{"id":101,"user":{"login":"alex"},"body":"Synthetic comment.","created_at":"` + fixtureAt(9*time.Hour) + `"}]`
		f.notifications = "[" + note("1", "author", "PullRequest", "https://api.github.com"+api12, t1) + "]"
	})
	if got := things(t, c); len(got) != 0 {
		t.Fatalf("unpersisted message was published: %+v", got)
	}
	store.fail = false
	replaceNewestMessageData(t, f, c, func() {})
	if got := things(t, c); got[newestMessageThread].Last == "" {
		t.Fatalf("notification was not retried after storage recovered: %+v", got)
	}
}

type messageLoadFailure struct {
	memSecrets
	fail bool
}

func (s *messageLoadFailure) Load(ctx context.Context) ([]byte, error) {
	if s.fail {
		return nil, errors.New("synthetic cursor load failure")
	}
	return s.memSecrets.Load(ctx)
}

func TestMessageCursorLoadFailureDoesNotOverwriteLogin(t *testing.T) {
	f, c := newestMessageFixture(t)
	store := &messageLoadFailure{fail: true}
	store.data = []byte(`{"token":"synthetic-login"}`)
	c.UseSecrets(store)
	replaceNewestMessageData(t, f, c, func() {
		f.comments["/repos/example-org/example-repo/issues/12/comments"] =
			`[{"id":101,"user":{"login":"alex"},"body":"Synthetic comment.","created_at":"` + fixtureAt(9*time.Hour) + `"}]`
	})
	if got := things(t, c); len(got) != 0 {
		t.Fatalf("unreadable cursor store was acknowledged: %+v", got)
	}
	if string(store.data) != `{"token":"synthetic-login"}` {
		t.Fatal("failed load overwrote the existing sign-in")
	}
	store.fail = false
	replaceNewestMessageData(t, f, c, func() {})
	var saved deviceLogin
	if err := json.Unmarshal(store.data, &saved); err != nil || saved.Token != "synthetic-login" {
		t.Fatalf("retry lost the existing sign-in: %v", err)
	}
	if got := things(t, c)[newestMessageThread]; got.Last == "" {
		t.Fatal("recovered store did not retry the notification")
	}
}

func TestMessageCursorSavePreservesLoginChanges(t *testing.T) {
	c := New(Config{}, "", nil)
	store := &memSecrets{data: []byte(`{"token":"synthetic-login"}`)}
	c.UseSecrets(store)
	last, _ := newestMessageLast([]move{{id: "comment:101", at: time.Now().UTC()}}, "")
	if err := c.rememberMessageLast(context.Background(), newestMessageThread, "alex", last); err != nil {
		t.Fatal(err)
	}
	if err := c.saveDevice(context.Background(), func(d *deviceLogin) { d.Token = "synthetic-replacement" }); err != nil {
		t.Fatal(err)
	}
	var saved deviceLogin
	if err := json.Unmarshal(store.data, &saved); err != nil || saved.Token != "synthetic-replacement" ||
		saved.InboxMessages[messageRecordKey(newestMessageThread, "alex")].Last != last {
		t.Fatalf("sign-in update dropped the cursor or token: %v", err)
	}
}

func TestMessageCursorsAreAccountScoped(t *testing.T) {
	c := New(Config{}, "", nil)
	store := &memSecrets{}
	c.UseSecrets(store)
	last, _ := newestMessageLast([]move{{id: "comment:101", at: time.Now().UTC()}}, "")
	if err := c.rememberMessageLast(context.Background(), newestMessageThread, "alex", last); err != nil {
		t.Fatal(err)
	}
	if c.previousMessageLast(context.Background(), newestMessageThread, "ALEX") != last {
		t.Fatal("the same account did not retain its cursor")
	}
	restarted := New(Config{}, "", nil)
	restarted.UseSecrets(store)
	if restarted.previousMessageLast(context.Background(), newestMessageThread, "blair") != "" {
		t.Fatal("another account inherited the previous account's cursor")
	}
	if restarted.previousMessageLast(context.Background(), newestMessageThread, "alex") != last {
		t.Fatal("restart lost the original account's cursor")
	}
}
