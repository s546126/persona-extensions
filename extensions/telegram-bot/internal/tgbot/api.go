package tgbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the official Telegram Bot API origin.
const DefaultBaseURL = "https://api.telegram.org"

const (
	maxAPIResponseBytes = 8 << 20
	maxImageBytes       = 5 << 20
	maxFileBytes        = 10 << 20
)

// APIError is an HTTP or Telegram Bot API error with the bot token redacted.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Code > 0 {
		return fmt.Sprintf("telegram api %d: %s", e.Code, e.Message)
	}
	return e.Message
}

func (e *APIError) Unauthorized() bool {
	return e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden
}

func (e *APIError) Retryable() bool {
	return e.Code == http.StatusTooManyRequests || e.Code >= http.StatusInternalServerError
}

// Client talks to the Telegram Bot API over HTTPS (optionally via an HTTP/HTTPS proxy).
type Client struct {
	base   string
	token  string
	proxy  string
	redact []string
	http   *http.Client
}

// NewClient constructs a Bot API client. proxyRaw may be empty (then http.ProxyFromEnvironment applies).
func NewClient(base, token, proxyRaw string) (*Client, error) {
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("invalid bot api base url")
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 40 * time.Second,
	}
	var redact []string
	addSecret := func(s string) {
		if s != "" {
			redact = append(redact, s)
		}
	}
	addSecret(token)
	if esc := url.PathEscape(token); esc != token {
		addSecret(esc)
	}
	proxyTrimmed := strings.TrimSpace(proxyRaw)
	if proxyTrimmed != "" {
		pu, perr := ParseProxyURL(proxyTrimmed)
		if perr != nil {
			return nil, perr
		}
		if pu.User != nil {
			if pw, ok := pu.User.Password(); ok {
				addSecret(pw)
				if esc := url.QueryEscape(pw); esc != pw {
					addSecret(esc)
				}
			}
			addSecret(pu.User.String())
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	return &Client{
		base:   u.String(),
		token:  token,
		proxy:  proxyTrimmed,
		redact: redact,
		http: &http.Client{
			Transport: tr,
			Timeout:   45 * time.Second,
			// Never follow redirects so the bot token in the request URL cannot leak to another origin.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// ParseProxyURL validates an optional HTTP or HTTPS proxy URL.
func ParseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("proxy must be http:// or https:// host:port")
	}
	return u, nil
}

// User is a Telegram Bot API User object.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

func (u User) DisplayName() string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name != "" {
		return name
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	if u.ID != 0 {
		return strconv.FormatInt(u.ID, 10)
	}
	return "Telegram user"
}

// Chat is a Telegram Bot API Chat object.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// PhotoSize is one size variant of a Telegram photo.
type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size"`
}

// Document is a general file attachment.
type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// Message is a Telegram Bot API Message object (fields we handle).
type Message struct {
	MessageID      int64           `json:"message_id"`
	From           *User           `json:"from"`
	Chat           Chat            `json:"chat"`
	Date           int64           `json:"date"`
	Text           string          `json:"text"`
	Caption        string          `json:"caption"`
	ReplyToMessage *Message        `json:"reply_to_message"`
	ForwardOrigin  json.RawMessage `json:"forward_origin,omitempty"`
	ForwardFrom    *User           `json:"forward_from,omitempty"`
	Photo          []PhotoSize     `json:"photo"`
	Document       *Document       `json:"document"`
	Voice          json.RawMessage `json:"voice,omitempty"`
	Audio          json.RawMessage `json:"audio,omitempty"`
	Video          json.RawMessage `json:"video,omitempty"`
	VideoNote      json.RawMessage `json:"video_note,omitempty"`
	Sticker        json.RawMessage `json:"sticker,omitempty"`
	Location       json.RawMessage `json:"location,omitempty"`
	Contact        json.RawMessage `json:"contact,omitempty"`
}

// Update is a Telegram Bot API Update object.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// File is returned by getFile.
type File struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	FilePath string `json:"file_path"`
}

type apiEnvelope struct {
	OK          bool            `json:"ok"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

func (c *Client) scrub(s string) string {
	for _, secret := range c.redact {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	if c.token != "" {
		s = strings.ReplaceAll(s, c.token, "[redacted]")
	}
	return s
}

func (c *Client) call(ctx context.Context, method string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, reqBody)
	if err != nil {
		return errors.New(c.scrub(err.Error()))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doJSON(req, out)
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New(c.scrub(err.Error()))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes+1))
	if err != nil {
		return errors.New(c.scrub(err.Error()))
	}
	if len(raw) > maxAPIResponseBytes {
		return errors.New("telegram response too large")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return &APIError{Code: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode >= 400 {
			return &APIError{Code: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		}
		return fmt.Errorf("invalid telegram response: %w", err)
	}
	if !env.OK {
		code := env.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &APIError{Code: code, Message: c.scrub(env.Description)}
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("decode telegram result: %w", err)
		}
	}
	return nil
}

// GetMe verifies the bot token and returns bot info.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", nil, &u)
	return u, err
}

// GetUpdates fetches incoming message updates using long polling.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	payload := map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message"},
	}
	var updates []Update
	err := c.call(ctx, "getUpdates", payload, &updates)
	return updates, err
}

// SendMessage sends a plain-text message and returns its message_id.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (int64, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if replyTo > 0 {
		payload["reply_parameters"] = map[string]any{
			"message_id":                  replyTo,
			"allow_sending_without_reply": true,
		}
	}
	var msg Message
	if err := c.call(ctx, "sendMessage", payload, &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

// SendPhoto uploads an image via multipart form-data and returns its message_id.
func (c *Client) SendPhoto(ctx context.Context, chatID int64, filename, mime string, data []byte, replyTo int64) (int64, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if replyTo > 0 {
		rp, _ := json.Marshal(map[string]any{
			"message_id":                  replyTo,
			"allow_sending_without_reply": true,
		})
		_ = w.WriteField("reply_parameters", string(rp))
	}
	filename = escapeQuotes(filename)
	if filename == "" {
		filename = "image.png"
	}
	mime = sanitizeHeaderValue(mime)
	if mime == "" {
		mime = "image/png"
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="photo"; filename="%s"`, filename))
	h.Set("Content-Type", mime)
	part, err := w.CreatePart(h)
	if err != nil {
		return 0, err
	}
	if _, err := part.Write(data); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/sendPhoto", &buf)
	if err != nil {
		return 0, errors.New(c.scrub(err.Error()))
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	var msg Message
	if err := c.doJSON(req, &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

func escapeQuotes(s string) string {
	s = sanitizeHeaderValue(s)
	s = strings.ReplaceAll(s, `\`, `_`)
	return strings.ReplaceAll(s, `"`, `_`)
}

func sanitizeHeaderValue(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
}

// SendChatAction sends a chat action such as "typing".
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	payload := map[string]any{
		"chat_id": chatID,
		"action":  action,
	}
	return c.call(ctx, "sendChatAction", payload, nil)
}

// GetFile fetches metadata and file_path for a file_id.
func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	var f File
	err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f)
	return f, err
}

// DownloadFile downloads a file from Telegram up to maxBytes.
func (c *Client) DownloadFile(ctx context.Context, filePath string, maxBytes int64) ([]byte, error) {
	rel := strings.TrimLeft(filePath, "/")
	if !validTelegramFilePath(rel) {
		return nil, errors.New("invalid telegram file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+c.token+"/"+rel, nil)
	if err != nil {
		return nil, errors.New(c.scrub(err.Error()))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New(c.scrub(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Code: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, errors.New(c.scrub(err.Error()))
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("file too large")
	}
	return data, nil
}

func validTelegramFilePath(rel string) bool {
	if rel == "" {
		return false
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f || r == '\\' || r == '?' || r == '#' {
			return false
		}
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}
