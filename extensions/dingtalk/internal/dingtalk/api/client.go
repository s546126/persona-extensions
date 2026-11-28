// Package api is a small client of DingTalk's public open platform for an internal app's robot: the app's access
// token, Stream mode (an outbound WebSocket that brings the robot's messages, so the hub needs no public address),
// sending to one person, downloading what a message carries, and answering through a message's session webhook.
// It follows the official Stream SDKs' wire behavior (open-dingtalk/dingtalk-stream-sdk-go and -python) without
// pulling them in. Secrets, tokens and message contents are never logged.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Origin is DingTalk's open platform.
const Origin = "https://api.dingtalk.com"

// requestTimeout bounds one REST call; downloads get downloadTimeout.
const (
	requestTimeout  = 15 * time.Second
	downloadTimeout = 2 * time.Minute
	// tokenEarly renews the access token this long before DingTalk says it expires.
	tokenEarly       = 5 * time.Minute
	maxAPIResponse   = 1 << 20
	maxTokenExpireIn = 24 * 60 * 60
)

// Options configure a Client. BaseURL and HTTP are what tests replace.
type Options struct {
	ClientID     string // the app's Client ID (AppKey); also the robot code of an internal app's robot
	ClientSecret string // the app's Client Secret (AppSecret)
	BaseURL      string // Origin when empty
	// HTTP makes every request (http.DefaultClient's transport, so HTTPS_PROXY is used, when nil). It must have no
	// Timeout: the WebSocket dial refuses one; every call bounds itself with a context instead.
	HTTP  *http.Client
	Agent string // the User-Agent product token ("Persona/<version>")
	// DialTimeout bounds connecting to the Stream endpoint (requestTimeout when 0); tests shorten it.
	DialTimeout time.Duration
	Now         func() time.Time
}

// Client talks to DingTalk for one app.
type Client struct {
	opt Options

	mu      sync.Mutex
	token   string
	expires time.Time
}

func New(opt Options) (*Client, error) {
	if strings.TrimSpace(opt.ClientID) == "" || strings.TrimSpace(opt.ClientSecret) == "" {
		return nil, errors.New("dingtalk: a client id and secret are needed")
	}
	if opt.BaseURL == "" {
		opt.BaseURL = Origin
	}
	u, err := url.Parse(opt.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("dingtalk: the open platform address must be https: %q", opt.BaseURL)
	}
	opt.BaseURL = strings.TrimRight(opt.BaseURL, "/")
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{CheckRedirect: noRedirects}
	} else {
		cl := *opt.HTTP
		cl.CheckRedirect = noRedirects
		opt.HTTP = &cl
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.DialTimeout <= 0 {
		opt.DialTimeout = requestTimeout
	}
	return &Client{opt: opt}, nil
}

// RobotCode is the robot's code: an internal app's robot has the app's Client ID.
func (c *Client) RobotCode() string { return c.opt.ClientID }

// Error is DingTalk's refusal: the HTTP status and its {code, message}. Message is DingTalk's and never holds a secret.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" && e.Message == "" {
		return fmt.Sprintf("DingTalk answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("DingTalk answered HTTP %d: %s %s", e.Status, e.Code, e.Message)
}

// ErrFlowControlled: DingTalk held the message back for the receiver (too many sent to them now); try again later.
var ErrFlowControlled = errors.New("DingTalk is holding messages to this person back for now")

// ErrNotReachable: DingTalk does not know the receiver for this robot (left the organization, not visible to the app).
var ErrNotReachable = errors.New("DingTalk can't deliver to this person from this robot")

// Retryable says a call may work if made again a little later: the network, DingTalk being busy, rate limits.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrFlowControlled) {
		return true
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Status == http.StatusTooManyRequests || e.Status >= 500
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded)
}

// NotSent says a send certainly did not reach DingTalk, so sending it again cannot duplicate it: DingTalk held it
// back (flow control, rate limit), or no connection was made.
func NotSent(err error) bool {
	if errors.Is(err, ErrFlowControlled) {
		return true
	}
	var e *Error
	if errors.As(err, &e) {
		// a rate limit, or a server error DingTalk answered with its own error code: it did not take the request.
		// A bare 5xx (a gateway with no DingTalk answer) may have been processed, and is not sent again
		return e.Status == http.StatusTooManyRequests || e.Status >= 500 && e.Code != ""
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// Refused says DingTalk answered and did not take the request (a 4xx other than a rate limit, or a receiver it
// cannot reach): it was not delivered.
func Refused(err error) bool {
	if errors.Is(err, ErrNotReachable) {
		return true
	}
	var e *Error
	return errors.As(err, &e) && e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests
}

// Unauthorized says DingTalk refused the app's credentials (a wrong or reset secret, a disabled app): a 401 or 403,
// or a 400 about the client or its secret.
func Unauthorized(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	code := strings.ToLower(e.Code)
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden ||
		e.Status == http.StatusBadRequest && (strings.Contains(code, "auth") || strings.Contains(code, "client") || strings.Contains(code, "secret"))
}

// call posts body as JSON to path on the open platform and reads the answer into out (nil: dropped).
func (c *Client) call(ctx context.Context, path string, token bool, body, out any) error {
	err := c.callOnce(ctx, path, token, body, out)
	var e *Error
	if token && errors.As(err, &e) && e.Status == http.StatusUnauthorized {
		// the token was expired or revoked before the local expiry: DingTalk refused it before doing anything, so
		// one more try with a fresh token cannot duplicate the call
		err = c.callOnce(ctx, path, token, body, out)
	}
	return err
}

func (c *Client) callOnce(ctx context.Context, path string, token bool, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opt.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.opt.Agent != "" {
		req.Header.Set("User-Agent", c.opt.Agent)
	}
	if token {
		t, err := c.Token(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("x-acs-dingtalk-access-token", t)
	}
	resp, err := c.opt.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse+1))
	if err != nil {
		return err
	}
	if len(data) > maxAPIResponse {
		return errors.New("DingTalk response body too large")
	}
	if resp.StatusCode != http.StatusOK {
		e := &Error{Status: resp.StatusCode}
		var m struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &m) == nil {
			e.Code, e.Message = m.Code, clip(m.Message, 200)
		}
		if token && resp.StatusCode == http.StatusUnauthorized {
			c.forgetToken()
		}
		return e
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Token is the app's access token, fetched again shortly before it expires (DingTalk gives two hours).
func (c *Client) Token(ctx context.Context) (string, error) {
	now := c.opt.Now()
	c.mu.Lock()
	if c.token != "" && now.Before(c.expires) {
		t := c.token
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int64  `json:"expireIn"`
	}
	if err := c.call(ctx, "/v1.0/oauth2/accessToken", false,
		map[string]string{"appKey": c.opt.ClientID, "appSecret": c.opt.ClientSecret}, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("DingTalk gave no access token")
	}
	expireIn := min(out.ExpireIn, int64(maxTokenExpireIn))
	life := time.Duration(expireIn) * time.Second
	if life <= tokenEarly {
		life = 2 * tokenEarly
	}
	c.mu.Lock()
	c.token, c.expires = out.AccessToken, now.Add(life-tokenEarly)
	c.mu.Unlock()
	return out.AccessToken, nil
}

func (c *Client) forgetToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// SendMarkdown sends one Markdown message from the robot to one person of the organization (their staff id), in
// their chat with the robot. DingTalk keeps no idempotency key: the caller sends a piece once.
func (c *Client) SendMarkdown(ctx context.Context, staffID, title, text string) error {
	param, err := json.Marshal(map[string]string{"title": title, "text": text})
	if err != nil {
		return err
	}
	var out struct {
		InvalidStaffIDList        []string `json:"invalidStaffIdList"`
		FlowControlledStaffIDList []string `json:"flowControlledStaffIdList"`
	}
	if err := c.call(ctx, "/v1.0/robot/oToMessages/batchSend", true, map[string]any{
		"robotCode": c.RobotCode(), "userIds": []string{staffID}, "msgKey": "sampleMarkdown", "msgParam": string(param),
	}, &out); err != nil {
		return err
	}
	switch {
	case slices.Contains(out.InvalidStaffIDList, staffID):
		return ErrNotReachable
	case slices.Contains(out.FlowControlledStaffIDList, staffID):
		return ErrFlowControlled
	}
	return nil
}

// Download fetches what a message's download code stands for (a picture, a file, a voice clip), refusing more than
// most bytes (ErrTooLarge).
func (c *Client) Download(ctx context.Context, downloadCode string, most int64) ([]byte, error) {
	var out struct {
		DownloadURL string `json:"downloadUrl"`
	}
	if err := c.call(ctx, "/v1.0/robot/messageFiles/download", true,
		map[string]string{"downloadCode": downloadCode, "robotCode": c.RobotCode()}, &out); err != nil {
		return nil, err
	}
	u, err := c.downloadURL(out.DownloadURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.opt.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Status: resp.StatusCode}
	}
	if resp.ContentLength > most {
		return nil, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, most+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > most {
		return nil, ErrTooLarge
	}
	return data, nil
}

// downloadURL checks where DingTalk says the bytes are: its own hosts or Alibaba Cloud OSS (where it keeps message
// files; seen in practice as http://<bucket>.oss-cn-<region>.aliyuncs.com/…). An http address is fetched over https:
// OSS serves the same object on both, and the bytes are someone's private message.
func (c *Client) downloadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("DingTalk gave no download address")
	}
	host := normalizeHost(u.Hostname())
	if !c.webhookHost(host) && host != "aliyuncs.com" && !strings.HasSuffix(host, ".aliyuncs.com") {
		return nil, fmt.Errorf("DingTalk's download address is on an unexpected host %q", u.Hostname())
	}
	u.Scheme = "https"
	return u, nil
}

// Redact is err for a log line without a request's URL: the signed download addresses and session webhooks DingTalk
// gives carry credentials in their query.
func Redact(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		host := ""
		if u, perr := url.Parse(ue.URL); perr == nil {
			host = u.Hostname()
		}
		return fmt.Sprintf("%s %s: %v", ue.Op, host, ue.Err)
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// ErrTooLarge: what a download brings is larger than allowed.
var ErrTooLarge = errors.New("too large")

// Reply answers in the chat a message came from, through its session webhook (valid for a while after the message;
// works before anyone is paired, for anyone who wrote to the robot). Text is sent as plain text.
func (c *Client) Reply(ctx context.Context, webhook, text string) error {
	u, err := url.Parse(webhook)
	if err != nil || u.Scheme != "https" || u.User != nil || !c.webhookHost(u.Hostname()) {
		return errors.New("not a DingTalk session webhook")
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	raw, _ := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": text}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.opt.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return &Error{Status: resp.StatusCode}
	}
	var m struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(data, &m) == nil && m.ErrCode != 0 {
		return &Error{Status: resp.StatusCode, Code: fmt.Sprint(m.ErrCode), Message: clip(m.ErrMsg, 200)}
	}
	return nil
}

// webhookHost says a session webhook may be posted to host: DingTalk's own (or, in tests, the base URL's host).
func (c *Client) webhookHost(host string) bool {
	host = normalizeHost(host)
	if base, err := url.Parse(c.opt.BaseURL); err == nil && normalizeHost(base.Hostname()) == host {
		return true
	}
	return host == "dingtalk.com" || strings.HasSuffix(host, ".dingtalk.com")
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
