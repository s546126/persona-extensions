package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/budget"
	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/store"
)

func TestHardeningServeRejectsOversizedLine(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"method":"ping","params":"` + strings.Repeat("a", (8<<20)+1024) + "\"}\n"
	var out bytes.Buffer
	err := Serve(context.Background(), strings.NewReader(huge), &out, store.Open(""), budget.Open(""), time.UTC, time.Now)
	if err == nil {
		t.Fatal("expected Serve to reject > 8 MiB line")
	}
	if !strings.Contains(err.Error(), "too large") && !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected oversized line error, got: %v", err)
	}
}
