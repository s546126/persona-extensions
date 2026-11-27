package ilink

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
)

// Code is a stable error class; messages never carry tokens, QR codes or message text.
type Code string

const (
	CodeAborted      Code = "aborted"
	CodeTimeout      Code = "timeout"
	CodeNetwork      Code = "network"
	CodeHTTP         Code = "http"
	CodeBadResponse  Code = "bad_response"
	CodeProtocol     Code = "protocol"
	CodeReplaced     Code = "replaced"      // another client took the bot token: authorize again
	CodeAuthExpired  Code = "auth_expired"  // the QR code expired too often, or the authorization is gone
	CodeAuthBlocked  Code = "auth_blocked"  // too many wrong verification codes
	CodeAlreadyBound Code = "already_bound" // this WeChat already has this bot
)

// Error is what the package returns; Retry says whether trying the same call again later can work.
type Error struct {
	Code   Code
	Msg    string
	Retry  bool
	status int
	err    error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.err }

// CodeOf is the Code of err, "" when err is not an *Error.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ErrorType narrows network failures into the small classes used by diagnostics, while other errors keep their
// stable iLink code.
func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	if code := CodeOf(err); code != "" && code != CodeNetwork {
		return string(code)
	}
	switch {
	case errors.Is(err, context.Canceled):
		return string(CodeAborted)
	case errors.Is(err, context.DeadlineExceeded):
		return string(CodeTimeout)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var recordHeader *tls.RecordHeaderError
	var certificate *tls.CertificateVerificationError
	var alert tls.AlertError
	if errors.As(err, &recordHeader) || errors.As(err, &certificate) || errors.As(err, &alert) {
		return "tls"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return "tcp"
	}
	if CodeOf(err) == CodeNetwork {
		return string(CodeNetwork)
	}
	return "unknown"
}

// Retryable says whether err may go away by itself.
func Retryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Retry
	}
	return true
}
