package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHardeningHubRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"machines":[{"id":"m1","name":"` + strings.Repeat("a", (8<<20)+1024) + `"}]}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "tok", nil).Machines(context.Background())
	if err == nil {
		t.Fatal("expected oversized response error")
	}
	if !strings.Contains(LogText(err), "too large") {
		t.Fatalf("expected explicit too large detail, got: %s", LogText(err))
	}
}
