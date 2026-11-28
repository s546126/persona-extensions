package serve

import (
	"context"
	"errors"
	"testing"
)

func TestWritesDoRespectsCallerContext(t *testing.T) {
	var w writes
	started := make(chan struct{})
	release := make(chan struct{})

	go func() {
		_, _ = w.doContext(context.Background(), "req-1", func() (CallResult, error) {
			close(started)
			<-release
			return CallResult{Text: "ok"}, nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.doContext(ctx, "req-1", func() (CallResult, error) {
		t.Fatal("duplicate request_id should wait on in-flight write, not re-run")
		return CallResult{}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter got %v, want context.Canceled", err)
	}
	close(release)
}
