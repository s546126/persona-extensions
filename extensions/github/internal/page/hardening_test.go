package page

import "testing"

func TestClipNonPositiveDoesNotPanic(t *testing.T) {
	if got := clip("hello", 0); got != "" {
		t.Fatalf("clip(\"hello\", 0) = %q, want empty", got)
	}
	if got := clip("hello", -3); got != "" {
		t.Fatalf("clip(\"hello\", -3) = %q, want empty", got)
	}
}
