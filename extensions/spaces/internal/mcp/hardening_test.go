package mcp

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/pluginkit/spaces/store"
)

func TestHardeningServeRejectsOversizedLine(t *testing.T) {
	// Stream > 8 MiB without a newline; Serve must fail promptly instead of buffering unboundedly.
	big := io.LimitReader(strings.NewReader(strings.Repeat("x", (9<<20))), 9<<20)
	var out bytes.Buffer
	err := Serve(context.Background(), big, &out, store.Open(""), time.Now)
	if err == nil {
		t.Fatalf("expected Serve to reject oversized (>8 MiB) JSON-RPC line")
	}
}
