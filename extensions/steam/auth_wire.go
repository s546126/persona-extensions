package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

const (
	authBasePath = "/IAuthenticationService/"
	beginQRPath  = "BeginAuthSessionViaQR"
	pollQRPath   = "PollAuthSessionStatus"
	renewPath    = "GenerateAccessTokenForApp"
	qrLifetime   = 5 * time.Minute
	minPoll      = 3 * time.Second
	maxAuthBytes = 64 << 10
)

var steamChallenge = regexp.MustCompile(`^https://s\.team/q/[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)*$`)

type qrChallenge struct {
	clientID  uint64
	requestID []byte
	url       string
	interval  time.Duration
}

type qrGrant struct {
	clientID  uint64
	url       string
	refresh   string
	access    string
	account   string
	agreement bool
}

// Valve's service_authentication protobuf messages use varints, length-delimited
// fields, and little-endian fixed32/fixed64 values.
func (c steamClient) beginQR(ctx context.Context) (qrChallenge, problem) {
	device := protoBytes(nil, 1, []byte("Persona"))
	device = protoVarint(device, 2, 3) // MobileApp
	raw, issue := c.authRPC(ctx, beginQRPath, protoBytes(nil, 3, device))
	if issue != "" {
		return qrChallenge{}, issue
	}
	var out qrChallenge
	var interval float64
	valid := protoFields(raw, func(f protoField) {
		switch {
		case f.number == 1 && f.wire == 0:
			out.clientID = f.value
		case f.number == 2 && f.wire == 2:
			out.url = string(f.data)
		case f.number == 3 && f.wire == 2:
			out.requestID = f.data
		case f.number == 4 && f.wire == 5:
			interval = float64(math.Float32frombits(uint32(f.value)))
		}
	})
	if !valid || out.clientID == 0 || len(out.requestID) == 0 || !validChallenge(out.url) ||
		math.IsNaN(interval) || math.IsInf(interval, 0) {
		return qrChallenge{}, problemResponse
	}
	// An interval longer than the entire login will never be reached.
	seconds := min(qrLifetime.Seconds(), max(minPoll.Seconds(), math.Ceil(interval)))
	out.interval = time.Duration(seconds) * time.Second
	return out, ""
}

func validChallenge(s string) bool { return len(s) <= 512 && steamChallenge.MatchString(s) }

func (c steamClient) pollQR(ctx context.Context, challenge qrChallenge) (qrGrant, problem) {
	msg := protoVarint(nil, 1, challenge.clientID)
	msg = protoBytes(msg, 2, challenge.requestID)
	raw, issue := c.authRPC(ctx, pollQRPath, msg)
	if issue != "" {
		return qrGrant{}, issue
	}
	var out qrGrant
	valid := protoFields(raw, func(f protoField) {
		switch {
		case f.number == 1 && f.wire == 0:
			out.clientID = f.value
		case f.number == 2 && f.wire == 2:
			out.url = string(f.data)
		case f.number == 3 && f.wire == 2:
			out.refresh = string(f.data)
		case f.number == 4 && f.wire == 2:
			out.access = string(f.data)
		case f.number == 6 && f.wire == 2:
			out.account = string(f.data)
		case f.number == 8 && f.wire == 2:
			out.agreement = len(f.data) > 0
		}
	})
	if !valid || (out.url != "" && !validChallenge(out.url)) {
		return qrGrant{}, problemAuthRefused
	}
	return out, ""
}

func (c steamClient) renewAccess(ctx context.Context, refresh, id string) (qrGrant, problem) {
	steamID, err := strconv.ParseUint(id, 10, 64)
	if err != nil || !validSteamID(id) {
		return qrGrant{}, problemResponse
	}
	msg := protoBytes(nil, 1, []byte(refresh))
	msg = binary.LittleEndian.AppendUint64(append(msg, 2<<3|1), steamID)
	msg = protoVarint(msg, 3, 1) // Allow refresh token renewal.
	raw, issue := c.authRPC(ctx, renewPath, msg)
	if issue == problemDenied {
		issue = problemAuthExpired
	}
	if issue != "" {
		return qrGrant{}, issue
	}
	var out qrGrant
	valid := protoFields(raw, func(f protoField) {
		switch {
		case f.number == 1 && f.wire == 2:
			out.access = string(f.data)
		case f.number == 2 && f.wire == 2:
			out.refresh = string(f.data)
		}
	})
	if !valid || out.access == "" {
		return qrGrant{}, problemResponse
	}
	return out, ""
}

func (c steamClient) authRPC(ctx context.Context, method string, message []byte) ([]byte, problem) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if form.WriteField("input_protobuf_encoded", base64.StdEncoding.EncodeToString(message)) != nil || form.Close() != nil {
		return nil, problemResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+authBasePath+method+"/v1/", &body)
	if err != nil {
		return nil, problemResponse
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, sanitizeError(err)
	}
	defer resp.Body.Close()
	if issue := httpProblem(resp.StatusCode); issue != "" {
		return nil, issue
	}
	result, err := strconv.Atoi(resp.Header.Get("X-EResult"))
	if err != nil {
		return nil, problemResponse
	}
	if issue := authResult(result); issue != "" {
		return nil, issue
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthBytes+1))
	if err != nil {
		return nil, sanitizeError(err)
	}
	if len(raw) > maxAuthBytes {
		return nil, problemResponse
	}
	return raw, ""
}

// EResult values are defined in Valve's steamclientpublic.h. Unknown failures
// stop a login rather than polling a potentially invalid challenge indefinitely.
func authResult(code int) problem {
	switch code {
	case 1: // OK
		return ""
	case 3, 10, 20: // NoConnection, Busy, ServiceUnavailable
		return problemNetwork
	case 16: // Timeout
		return problemTimeout
	case 22: // Pending
		return problemAuthPending
	case 84, 87: // RateLimitExceeded, AccountLoginDeniedThrottle
		return problemRateLimited
	case 5, 15, 21, 26, 27: // InvalidPassword, AccessDenied, NotLoggedOn, Revoked, Expired
		return problemAuthExpired
	default:
		return problemAuthRefused
	}
}

func protoVarint(dst []byte, field, value uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(dst, field<<3), value)
}

func protoBytes(dst []byte, field uint64, value []byte) []byte {
	dst = binary.AppendUvarint(dst, field<<3|2)
	return append(binary.AppendUvarint(dst, uint64(len(value))), value...)
}

type protoField struct {
	number uint64
	wire   uint64
	value  uint64
	data   []byte
}

func protoFields(raw []byte, visit func(protoField)) bool {
	for len(raw) != 0 {
		tag, n := binary.Uvarint(raw)
		if n <= 0 || tag>>3 == 0 || tag>>3 >= 1<<29 {
			return false
		}
		raw = raw[n:]
		f := protoField{number: tag >> 3, wire: tag & 7}
		switch f.wire {
		case 0:
			f.value, n = binary.Uvarint(raw)
			if n <= 0 {
				return false
			}
		case 1:
			n = 8
			if len(raw) < n {
				return false
			}
			f.value = binary.LittleEndian.Uint64(raw)
		case 2:
			size, prefix := binary.Uvarint(raw)
			if prefix <= 0 || size > uint64(len(raw)-prefix) {
				return false
			}
			n = prefix + int(size)
			f.data = raw[prefix:n]
		case 5:
			n = 4
			if len(raw) < n {
				return false
			}
			f.value = uint64(binary.LittleEndian.Uint32(raw))
		default:
			return false
		}
		visit(f)
		raw = raw[n:]
	}
	return true
}
