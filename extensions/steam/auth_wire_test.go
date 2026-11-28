package main

import (
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestSteamQRWire(t *testing.T) {
	// Valve's protobuf layout, with an eight-byte client ID, opaque bytes and a 3.5s interval.
	const challenge = "https://s.team/q/1/fixture"
	f := newAuthServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch r.URL.Path {
		case authBasePath + "BeginAuthSessionViaQR/v1/":
			if hex.EncodeToString(body) != "1a0b0a07506572736f6e611003" {
				t.Error("expected Persona and MobileApp inside device_details")
			}
			w.Header().Set("X-EResult", "1")
			_, _ = w.Write(mustHex(t, "088180808080808010121a68747470733a2f2f732e7465616d2f712f312f666978747572651a0300ff0125000060402a020802"))
		case authBasePath + "PollAuthSessionStatus/v1/":
			if hex.EncodeToString(body) != "088180808080808010120300ff01" {
				t.Error("expected the original uint64 client ID and request bytes")
			}
			w.Header().Set("X-EResult", "1")
			_, _ = w.Write(mustHex(t, "082a121468747470733a2f2f732e7465616d2f712f6e6577"))
		case authBasePath + "GenerateAccessTokenForApp/v1/":
			if hex.EncodeToString(body) != "0a0f666978747572655f7265667265736811ffffffff010010011801" {
				t.Error("expected refresh token, little-endian fixed64 SteamID and Allow renewal")
			}
			w.Header().Set("X-EResult", "1")
			_, _ = w.Write(mustHex(t, "0a0e666978747572655f616363657373120b666978747572655f6e6577"))
		default:
			t.Error("unexpected authentication request")
		}
	})
	c := steamClient{http: f}
	b, issue := c.beginQR(t.Context())
	if issue != "" || b.url != challenge || b.interval != 4*time.Second {
		t.Fatalf("BeginAuthSessionViaQR parsing failed, issue %q", issue)
	}
	p, issue := c.pollQR(t.Context(), b)
	if issue != "" || p.clientID != 42 || p.url != "https://s.team/q/new" {
		t.Fatalf("PollAuthSessionStatus parsing failed, issue %q", issue)
	}
	g, issue := c.renewAccess(t.Context(), "fixture_refresh", fixtureID)
	if issue != "" || g.access != "fixture_access" || g.refresh != "fixture_new" {
		t.Fatalf("GenerateAccessTokenForApp parsing failed, issue %q", issue)
	}
}

func TestSteamAuthRequiresEResult(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		status     int
		want       problem
	}{
		{"pending", "22", 200, problemAuthPending},
		{"busy", "10", 200, problemNetwork},
		{"throttled", "84", 200, problemRateLimited},
		{"revoked", "26", 200, problemAuthExpired},
		{"refused", "8", 200, problemAuthRefused},
		{"missing", "", 200, problemResponse},
		{"bad header", "synthetic-secret", 200, problemResponse},
		{"HTTP error", "1", 503, problemNetwork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
				w.Header().Set("X-EResult", tc.code)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "synthetic upstream error")
			})
			_, issue := (steamClient{http: f}).pollQR(t.Context(), qrChallenge{clientID: 2, requestID: []byte("synthetic")})
			if issue != tc.want {
				t.Errorf("got %q, want %q", issue, tc.want)
			}
		})
	}
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	b, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newAuthServer(t *testing.T, handle func(http.ResponseWriter, *http.Request, []byte)) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 16); err != nil {
			t.Error("expected a multipart protobuf request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, err := base64.StdEncoding.DecodeString(r.FormValue("input_protobuf_encoded"))
		if err != nil || len(body) == 0 {
			t.Error("expected a base64 protobuf payload")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		handle(w, r, body)
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return steamHTTP(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Host != apiHost || req.URL.Scheme != "https" ||
			req.URL.RawQuery != "" || req.Header.Get("X-WebAPI-Key") != "" {
			t.Error("auth RPC left the expected HTTPS origin, method or credential boundary")
		}
		r := req.Clone(req.Context())
		r.URL.Host = target.Host
		return server.Client().Transport.RoundTrip(r)
	}), time.Second)
}
