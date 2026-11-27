package api

import (
	"net/http"
	"testing"
)

type recordKeys struct{ keys []string }

func (r *recordKeys) RoundTrip(req *http.Request) (*http.Response, error) {
	for k := range req.Header {
		r.keys = append(r.keys, k)
	}
	return &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}, Request: req}, nil
}

// WeCom answers 404 to Go's canonical "Sec-Websocket-Key" (it matches header names case-sensitively).
func TestHandshakeHeaderNamesAreSpelledAsTheRFC(t *testing.T) {
	rec := &recordKeys{}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	req.Header.Set("Sec-WebSocket-Key", "k")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Upgrade", "websocket")
	if _, ok := req.Header["Sec-Websocket-Key"]; !ok {
		t.Fatal("Go no longer canonicalizes the name; this test needs another look")
	}
	_, _ = wsCasing{rec}.RoundTrip(req)
	want := map[string]bool{"Sec-WebSocket-Key": true, "Sec-WebSocket-Version": true, "Upgrade": true}
	if len(rec.keys) != len(want) {
		t.Fatalf("keys %q", rec.keys)
	}
	for _, k := range rec.keys {
		if !want[k] {
			t.Errorf("sent %q", k)
		}
	}
	if _, ok := req.Header["Sec-Websocket-Key"]; !ok {
		t.Error("the caller's request was changed")
	}
}
