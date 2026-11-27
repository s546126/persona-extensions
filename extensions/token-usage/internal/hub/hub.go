// Package hub is the extension's view of the hub's API: the machine list, one machine's token usage, the gateway
// account list and a notification. It uses the token the hub handed the extension, whose scopes are machines.read and
// notify; the gateway list is an extension method, so it goes through the process protocol instead.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	stdsync "sync"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"

	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/store"
)

// Machine is one computer the hub lists.
type Machine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
}

// Series is one machine's usage over a range, as GET /api/v1/machines/{id}/usage answers it.
type Series struct {
	MachineID string  `json:"machine_id"`
	Range     string  `json:"range"`
	Step      int     `json:"step"`
	Points    []Point `json:"points"`
	// Stored: the hub keeps the week (Postgres). False: it holds only what is in memory, the last 26 hours, so the
	// 7d range reads zero before that whether or not anything was used.
	Stored bool `json:"stored"`
}

// Point is one step of a series. Step is the step's length in seconds, copied from the series, so a point knows its
// own minutes.
type Point struct {
	T    int64 `json:"t"`
	Step int   `json:"-"`
	In   int64 `json:"in"`
	CW   int64 `json:"cache_write"`
	CR   int64 `json:"cache_read"`
	Out  int64 `json:"out"`
	Req  int   `json:"req"`
	Gap  bool  `json:"gap,omitempty"`
}

// Share is one model's or launcher's sums, used only to split a point back into minutes.
type Share struct {
	Name string `json:"name"`
	In   int64  `json:"in"`
	CW   int64  `json:"cache_write"`
	CR   int64  `json:"cache_read"`
	Out  int64  `json:"out"`
	Req  int    `json:"req"`
}

// GatewayAccounts is the answer of usage.gateway_accounts.
type GatewayAccounts struct {
	Sources  []GatewaySource  `json:"sources"`
	Accounts []GatewayAccount `json:"accounts"`
}

// GatewaySource is one usage source.
type GatewaySource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Error    string `json:"error"`
	Listed   int    `json:"listed"`
	Complete bool   `json:"complete"`
}

// GatewayAccount is one account of a gateway.
type GatewayAccount struct {
	Source   string          `json:"source"`
	Name     string          `json:"name"`
	Provider string          `json:"provider"`
	Status   string          `json:"status"`
	Stale    bool            `json:"stale"`
	Windows  []GatewayWindow `json:"windows"`
}

// GatewayWindow is one quota window.
type GatewayWindow struct {
	Kind      string   `json:"kind"`
	UsedRatio *float64 `json:"used_ratio"`
}

// Client talks to the hub.
type Client struct {
	base, token string
	http        *http.Client
	call        func(ctx context.Context, method string, out any) error // the extension methods; nil when there is no hub

	mu   stdsync.Mutex
	last PullReport // how the last pull round ended
}

// PullReport is how a pull round ended. Err is set when nothing could be read (the computer list failed); otherwise
// Failed lists the computers whose usage could not be read or saved, out of Total.
type PullReport struct {
	At     time.Time
	Err    error
	Total  int
	Failed []FailedMachine
}

// FailedMachine is one computer a round could not read: its display name and why.
type FailedMachine struct {
	Name string
	Err  error
}

// NotFound reports whether err is the hub answering 404, which for one computer's usage means it is gone.
func NotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// Error is a failed call told for the user: Error() says what happened and what to do, in plain words, without the
// technical detail; Detail is that detail (status, body, network error) for the log.
type Error struct {
	Status  int // the HTTP status, 0 when there was no answer
	Message string
	Detail  string
}

func (e *Error) Error() string { return e.Message }

// LogText is err with its technical detail, for the log.
func LogText(err error) string {
	var e *Error
	if errors.As(err, &e) && e.Detail != "" {
		return e.Message + "（" + e.Detail + "）"
	}
	return err.Error()
}

// The messages the page shows. They name "运行 Persona 的那台电脑", the computer the hub runs on, and what to do.
const (
	msgNoAccess    = "运行 Persona 的那台电脑没有给这个扩展读取用量的权限，所以这里读不到数据。请到 设置 › 扩展 › Token 用量，把它关掉再打开；还不行就重启 Persona。"
	msgUnreachable = "这个扩展暂时连不上 Persona，可能 Persona 正在重启或更新。这里每 10 分钟会自动再试；一直这样的话，请重启 Persona。"
	msgBadAddress  = "这个扩展拿到的 Persona 地址用不了，所以发不出读取请求。请到 设置 › 扩展 › Token 用量，把它关掉再打开；还不行就重启 Persona。"
	msgCutOff      = "读取用量时连接中途断了，没读完。这里每 10 分钟会自动再试，不用做别的。"
	msgBadAnswer   = "运行 Persona 的那台电脑回来的内容这个扩展读不懂，可能 Persona 正在更新，或者它和这个扩展的版本不配套。这里每 10 分钟会自动再试；一直这样的话，请把 Persona 和这个扩展都更新到最新版。"
	msgRefused     = "运行 Persona 的那台电脑拒绝了读取用量（权限不够或已过期）。请到 设置 › 扩展 › Token 用量，把它关掉再打开。"
	msgTooOld      = "运行 Persona 的那台电脑不认识读取用量的请求，多半是 Persona 版本太旧。请把那台电脑上的 Persona 更新到最新版。"
	msgBusy        = "运行 Persona 的那台电脑暂时出了问题（错误码 %d）。不用处理，这里每 10 分钟会自动再试。"
	msgOther       = "运行 Persona 的那台电脑没有接受读取用量的请求（错误码 %d）。这里每 10 分钟会自动再试；一直这样的话，请把 Persona 更新到最新版。"
)

// statusError is the user's words for an HTTP status the hub answered with.
func statusError(status int, body string) *Error {
	e := &Error{Status: status, Detail: fmt.Sprintf("HTTP %d：%s", status, body)}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Message = msgRefused
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented:
		e.Message = msgTooOld
	case status >= 500 || status == http.StatusTooManyRequests:
		e.Message = fmt.Sprintf(msgBusy, status)
	default:
		e.Message = fmt.Sprintf(msgOther, status)
	}
	return e
}

// ReportPull records how a pull round ended.
func (c *Client) ReportPull(r PullReport) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = r
}

// LastPull is how the last pull round ended; a zero At when none has ended yet. A client that cannot call the hub at
// all answers that error at once, so the page can say so before any round.
func (c *Client) LastPull() PullReport {
	if !c.Usable() {
		return PullReport{Err: &Error{Message: msgNoAccess, Detail: "no hub address or token"}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// New returns a client for the hub at base, authorised by token. call runs an extension method such as
// usage.gateway_accounts; nil when the program has no hub connection (the assistant's server never has one).
func New(base, token string, call func(ctx context.Context, method string, out any) error) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
		call:  call,
	}
}

// NewFromHub returns a client on the connection the hub gave the program and the token it granted.
func NewFromHub(h *ext.Hub, base, token string) *Client {
	return New(base, token, func(ctx context.Context, method string, out any) error {
		return h.Call(ctx, method, map[string]any{}, out)
	})
}

// Usable reports whether the client can call the hub's HTTP API.
func (c *Client) Usable() bool { return c != nil && c.base != "" && c.token != "" }

// Machines lists the computers.
func (c *Client) Machines(ctx context.Context) ([]Machine, error) {
	var out struct {
		Machines []Machine `json:"machines"`
	}
	if err := c.get(ctx, "/api/v1/machines", &out); err != nil {
		return nil, err
	}
	return out.Machines, nil
}

// Usage reads one machine's usage over a range (1h, 24h or 7d). model and launcher narrow it; launcher "-" is the
// usage with no launcher.
func (c *Client) Usage(ctx context.Context, id, rng, model, launcher string) (Series, error) {
	q := url.Values{"range": {rng}}
	if model != "" {
		q.Set("model", model)
	}
	if launcher != "" {
		q.Set("launcher", launcher)
	}
	var out struct {
		Usage Series `json:"usage"`
	}
	err := c.get(ctx, "/api/v1/machines/"+url.PathEscape(id)+"/usage?"+q.Encode(), &out)
	for i := range out.Usage.Points {
		out.Usage.Points[i].Step = out.Usage.Step
	}
	return out.Usage, err
}

// Shares reads one machine's per-model and per-launcher sums over a range.
func (c *Client) Shares(ctx context.Context, id, rng string) (models, launchers []Share, err error) {
	var out struct {
		Usage struct {
			Models    []Share `json:"models"`
			Launchers []Share `json:"launchers"`
		} `json:"usage"`
	}
	err = c.get(ctx, "/api/v1/machines/"+url.PathEscape(id)+"/usage?range="+url.QueryEscape(rng), &out)
	return out.Usage.Models, out.Usage.Launchers, err
}

// Pull reads one machine's usage over rng and returns it as minute buckets, one per model and launcher. It asks for
// the range once to learn the models and launchers, then once per pair, so each bucket's totals are the hub's own
// numbers for that pair. launcher "-" in a share's name is the usage with no launcher. A zero point corrects the stored
// minutes only where the hub holds the whole range (see bucketsOf).
func (c *Client) Pull(ctx context.Context, id, rng string) ([]store.Bucket, error) {
	models, launchers, err := c.Shares(ctx, id, rng)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, nil
	}
	if len(launchers) == 0 {
		launchers = []Share{{Name: "-"}}
	}
	var out []store.Bucket
	for _, m := range models {
		for _, l := range launchers {
			s, err := c.Usage(ctx, id, rng, m.Name, l.Name)
			if err != nil {
				return nil, err
			}
			name := l.Name
			if name == "-" {
				name = ""
			}
			out = append(out, bucketsOf(s, m.Name, name, s.Stored || rng == "1h" || rng == "24h")...)
		}
	}
	return out, nil
}

// GatewayAccounts reads the gateway account list. ok is false when the hub has no such method or refused it, so the
// caller hides that part of the page instead of showing an error.
func (c *Client) GatewayAccounts(ctx context.Context) (GatewayAccounts, bool) {
	var out GatewayAccounts
	if c == nil || c.call == nil {
		return out, false
	}
	if err := c.call(ctx, "usage.gateway_accounts", &out); err != nil {
		return GatewayAccounts{}, false
	}
	if len(out.Accounts) == 0 && len(out.Sources) == 0 {
		return out, false
	}
	return out, true
}

// Notify sends one notification. A hub that answers sent=false (a muted device) is not an error.
func (c *Client) Notify(ctx context.Context, title, body, id string) error {
	return c.post(ctx, "/api/v1/notify", map[string]string{
		"title": title, "body": body, "level": "active", "id": id,
	})
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPost, path, body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if !c.Usable() {
		return &Error{Message: msgNoAccess, Detail: "no hub address or token"}
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return &Error{Message: msgBadAnswer, Detail: "encode request: " + err.Error()}
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return &Error{Message: msgBadAddress, Detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Message: msgUnreachable, Detail: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return &Error{Status: resp.StatusCode, Message: msgCutOff, Detail: "read body: " + err.Error()}
	}
	if resp.StatusCode >= 300 {
		return statusError(resp.StatusCode, clipBytes(strings.TrimSpace(string(raw)), 200))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return &Error{Status: resp.StatusCode, Message: msgBadAnswer,
				Detail: "decode answer: " + err.Error() + "：" + clipBytes(strings.TrimSpace(string(raw)), 200)}
		}
	}
	return nil
}

// bucketsOf turns one filtered series into minute buckets of one model and launcher. A point covers step seconds,
// so its tokens are spread evenly over its minutes; the first minute keeps the remainder and the requests, and the
// minutes add back to the point. Every minute of a point is kept, zero ones too, so it replaces all of the point's
// stored minutes. A gap is skipped. An empty point is kept to delete a minute the hub has corrected to zero only when
// zeros is true, that is when the hub holds the whole range; otherwise it may only mean the hub no longer has it.
func bucketsOf(s Series, model, launcher string, zeros bool) []store.Bucket {
	var out []store.Bucket
	for _, p := range s.Points {
		if p.Gap || !zeros && p.In+p.CW+p.CR+p.Out == 0 && p.Req == 0 {
			continue
		}
		minutes := p.Step / 60
		if minutes < 1 {
			minutes = 1
		}
		for k := 0; k < minutes; k++ {
			b := store.Bucket{
				Minute:     p.T + int64(k*60),
				Model:      model,
				Launcher:   launcher,
				Input:      part(p.In, minutes, k),
				CacheWrite: part(p.CW, minutes, k),
				CacheRead:  part(p.CR, minutes, k),
				Output:     part(p.Out, minutes, k),
			}
			if k == 0 {
				b.Requests = p.Req
			}
			out = append(out, b)
		}
	}
	return out
}

// part is one minute's share of a point's total. The first minute keeps the remainder.
func part(total int64, minutes, minute int) int64 {
	if total == 0 {
		return 0
	}
	each := total / int64(minutes)
	if minute == 0 {
		return total - int64(minutes-1)*each
	}
	return each
}

// clipBytes cuts s to at most n bytes without splitting a UTF-8 sequence: it backs off from byte n to the start of the
// rune there (at most utf8.UTFMax-1 bytes, so bytes that are not UTF-8 are still cut near n).
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for i := 0; i < utf8.UTFMax-1 && n > 0 && !utf8.RuneStart(s[n]); i++ {
		n--
	}
	return s[:n]
}
