package x

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
		chunk := strings.Repeat(" ", 64*1024)
		for range 257 { // 257 * 64 KiB > 16 MiB
			_, _ = w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	c := &client{
		base:   srv.URL,
		http:   srv.Client(),
		now:    time.Now,
		limits: map[string]limit{},
	}
	var out map[string]any
	err := c.get(t.Context(), Keys{
		APIKey: "ck", APISecret: "cs", AccessToken: "at", AccessTokenSecret: "as",
	}, "/2/users/me", nil, &out)
	if err == nil {
		t.Fatal("client silently accepted oversized >16 MiB response")
	}
}
