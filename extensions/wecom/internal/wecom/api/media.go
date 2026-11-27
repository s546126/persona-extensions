package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Media (pictures, files, videos) come as a URL valid for five minutes and a per-message AES key: the bytes are
// AES-256-CBC encrypted, the IV is the key's first 16 bytes, and the plaintext is PKCS#7 padded to a multiple of 32.

// DefaultMediaHosts are where WeCom keeps message media (its own and Tencent Cloud's hosts).
var DefaultMediaHosts = []string{"*.qq.com", "*.myqcloud.com", "*.qpic.cn"}

const downloadTimeout = 2 * time.Minute

// ErrTooLarge: what a download brings is larger than allowed.
var ErrTooLarge = errors.New("too large")

// Download fetches a message's media from rawURL and decrypts it with aesKey, refusing more than most bytes. name is
// the file's name as the download gives it (Content-Disposition): a file message carries none of its own.
func (c *Client) Download(ctx context.Context, rawURL, aesKey string, most int64) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.User != nil || u.Scheme != "https" && u.Scheme != "http" {
		return nil, "", errors.New("WeCom gave no download address")
	}
	if !hostAllowed(u.Hostname(), c.opt.MediaHosts) {
		return nil, "", fmt.Errorf("WeCom's download address is on an unexpected host %q", u.Hostname())
	}
	u.Scheme = "https" // the bytes are someone's private message
	key, err := mediaKey(aesKey)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.mediaClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("WeCom's media answered HTTP %d", resp.StatusCode)
	}
	limit := most + 32 // the padding
	if resp.ContentLength > limit {
		return nil, "", ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", ErrTooLarge
	}
	plain, err := Decrypt(data, key)
	if err != nil {
		return nil, "", err
	}
	if int64(len(plain)) > most {
		return nil, "", ErrTooLarge
	}
	return plain, fileName(resp.Header.Get("Content-Disposition")), nil
}

// fileName is the file name a Content-Disposition gives (filename* first), its directories taken off; "" for none.
func fileName(disposition string) string {
	_, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(params["filename"]) // mime decodes filename* into filename
	// WeCom puts a non-ASCII name into the plain filename URL-encoded (spaces as "+"), seen 2026-11
	if strings.Contains(name, "%") {
		if d, err := url.QueryUnescape(name); err == nil && utf8.ValidString(d) {
			name = d
		}
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
}

// mediaClient is the HTTP client for media: every redirect must stay on HTTPS and on the media hosts.
func (c *Client) mediaClient() *http.Client {
	hc := *c.opt.HTTP
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("WeCom's media redirected too many times")
		}
		if req.URL.Scheme != "https" || req.URL.User != nil || !hostAllowed(req.URL.Hostname(), c.opt.MediaHosts) {
			return fmt.Errorf("WeCom's media redirected to an unexpected place %q", req.URL.Hostname())
		}
		return nil
	}
	return &hc
}

// Redact is err for a log line without a request's URL: media addresses carry signed queries.
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

// mediaKey is the 32-byte key an aeskey stands for: base64 (WeCom's keys are 43 characters without the padding).
func mediaKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil && len(k) == 32 {
			return k, nil
		}
		if k, err := enc.DecodeString(s + "="); err == nil && len(k) == 32 {
			return k, nil
		}
	}
	if len(s) == 32 {
		return []byte(s), nil
	}
	return nil, errors.New("WeCom gave a media key that does not read")
}

// Decrypt undoes WeCom's media encryption (AES-256-CBC, IV the key's first 16 bytes, PKCS#7 to 32-byte blocks).
func Decrypt(data, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("a media key must be 32 bytes")
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("WeCom's media is not whole AES blocks")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(out, data)
	n := int(out[len(out)-1])
	if n < 1 || n > 32 || n > len(out) {
		return nil, errors.New("WeCom's media did not decrypt (bad padding)")
	}
	for _, b := range out[len(out)-n:] {
		if int(b) != n {
			return nil, errors.New("WeCom's media did not decrypt (bad padding)")
		}
	}
	return out[:len(out)-n], nil
}

// Encrypt is WeCom's media encryption, for tests.
func Encrypt(plain, key []byte) []byte {
	n := 32 - len(plain)%32
	padded := append(append([]byte(nil), plain...), bytesOf(byte(n), n)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, padded)
	return out
}

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// hostAllowed says host matches one of hosts ("example.com" exactly, "*.example.com" any subdomain).
func hostAllowed(host string, hosts []string) bool {
	for _, h := range hosts {
		if suffix, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == h {
			return true
		}
	}
	return false
}
