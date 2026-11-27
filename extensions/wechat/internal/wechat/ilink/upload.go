package ilink

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sending a picture follows openclaw-weixin's upload (src/cdn/upload.ts, cdn-upload.ts, messaging/send.ts): the
// picture is encrypted with a fresh AES-128 key, iLink's getuploadurl names where it goes on WeChat's CDN, the CDN
// answers the download reference in x-encrypted-param, and an image_item names that reference and the key.

const (
	uploadTimeout = 60 * time.Second
	mediaImage    = 1 // getuploadurl's media_type for a picture (UploadMediaType.IMAGE)
	// cdnAnswerMost is how much of the CDN's answer body is read (and dropped) so its connection can be used again.
	cdnAnswerMost = 64 << 10
)

// cdnUploadBase is where a picture goes when getuploadurl names only an encrypted parameter.
var cdnUploadBase = cdnOrigin + "/c2c"

// Uploaded is a picture on WeChat's CDN that SendImage can name.
type Uploaded struct {
	// Query is the CDN's download reference (x-encrypted-param).
	Query string
	// KeyHex is the picture's AES-128 key as 32 hex digits.
	KeyHex string
	// CipherSize is the encrypted picture's size in bytes.
	CipherSize int
}

// UploadImage encrypts a picture (at most MaxMedia bytes) with a fresh key and uploads it to WeChat's CDN for peer.
func (c *Client) UploadImage(ctx context.Context, peer string, data []byte) (Uploaded, error) {
	if strings.TrimSpace(peer) == "" || len(data) == 0 || len(data) > MaxMedia {
		return Uploaded{}, &Error{Code: CodeProtocol, Msg: "incomplete iLink picture"}
	}
	var key, fileKey [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		return Uploaded{}, err
	}
	if _, err := rand.Read(fileKey[:]); err != nil {
		return Uploaded{}, err
	}
	cipher, err := EncryptECB(data, key[:])
	if err != nil {
		return Uploaded{}, err
	}
	sum := md5.Sum(data)
	keyHex, fileKeyHex := hex.EncodeToString(key[:]), hex.EncodeToString(fileKey[:])
	out, err := c.call(ctx, http.MethodPost, "ilink/bot/getuploadurl", map[string]any{
		"filekey": fileKeyHex, "media_type": mediaImage, "to_user_id": peer, "rawsize": len(data),
		"rawfilemd5": hex.EncodeToString(sum[:]), "filesize": len(cipher), "no_need_thumb": true, "aeskey": keyHex,
		"base_info": c.baseInfo(),
	}, true, apiTimeout, nil)
	if err != nil {
		return Uploaded{}, err
	}
	if err := refusal(out, "the picture's upload"); err != nil {
		return Uploaded{}, err
	}
	target, err := uploadURL(strings.TrimSpace(str(out, "upload_full_url")), str(out, "upload_param"), fileKeyHex)
	if err != nil {
		return Uploaded{}, err
	}
	// One try: the caller tries a failure that may pass again, waiting between tries, with a fresh upload URL. The
	// request carries no Authorization: the bot token is iLink's, not the CDN's.
	query, err := c.putCDN(ctx, target, cipher)
	if err != nil {
		return Uploaded{}, err
	}
	return Uploaded{Query: query, KeyHex: keyHex, CipherSize: len(cipher)}, nil
}

// uploadURL is where the picture goes: getuploadurl's full URL (a Tencent HTTPS URL only), else the CDN's upload
// with its encrypted parameter.
func uploadURL(full, param, fileKey string) (string, error) {
	if full != "" {
		u, err := url.Parse(full)
		if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || !tencentHost(u.Hostname()) {
			return "", &Error{Code: CodeProtocol, Msg: "upload URL is not a Tencent HTTPS URL (" + formatURLDiag("upload_url", u, err, false) + ")"}
		}
		return u.String(), nil
	}
	if param == "" {
		return "", &Error{Code: CodeBadResponse, Msg: "iLink named no upload URL for the picture", Retry: true}
	}
	return cdnUploadBase + "/upload?" + url.Values{"encrypted_query_param": {param}, "filekey": {fileKey}}.Encode(), nil
}

// putCDN posts the encrypted picture to target and returns the download reference the CDN answers.
func (c *Client) putCDN(ctx context.Context, target string, cipher []byte) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, target, bytes.NewReader(cipher))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded): // the caller's deadline, reported as call reports it
			return "", &Error{Code: CodeTimeout, Msg: "picture upload timed out", Retry: true}
		case ctx.Err() != nil:
			return "", &Error{Code: CodeAborted, Msg: "picture upload cancelled", Retry: true}
		case errors.Is(cctx.Err(), context.DeadlineExceeded):
			return "", &Error{Code: CodeTimeout, Msg: "picture upload timed out", Retry: true}
		}
		return "", &Error{Code: CodeNetwork, Msg: "picture upload failed", Retry: true, err: err}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, cdnAnswerMost))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &Error{Code: CodeHTTP, Msg: fmt.Sprintf("picture upload answered HTTP %d", resp.StatusCode),
			Retry: resp.StatusCode >= 500 || resp.StatusCode == 429}
	}
	query := strings.TrimSpace(resp.Header.Get("x-encrypted-param"))
	if query == "" {
		return "", &Error{Code: CodeBadResponse, Msg: "picture upload answered no download reference", Retry: true}
	}
	return query, nil
}

// SendImage sends an uploaded picture to peer as the bot, in the chat of contextToken; clientID makes the send
// idempotent on iLink's side. It returns the server message id, which a later ref_msg.svr_id can reference.
func (c *Client) SendImage(ctx context.Context, peer, contextToken, clientID string, u Uploaded) (string, error) {
	if strings.TrimSpace(peer) == "" || strings.TrimSpace(contextToken) == "" || strings.TrimSpace(clientID) == "" ||
		u.Query == "" || !hex32.MatchString(u.KeyHex) || u.CipherSize <= 0 {
		return "", &Error{Code: CodeProtocol, Msg: "incomplete iLink picture message"}
	}
	item := map[string]any{"type": itemImage, "image_item": map[string]any{
		// The key goes both ways WeChat carries it: media.aes_key is base64 of the key's hex text, as openclaw-weixin
		// sends it, and the image's own aeskey is the hex key, which is where a received picture has it (message.go).
		"media":    map[string]any{"encrypt_query_param": u.Query, "aes_key": base64.StdEncoding.EncodeToString([]byte(u.KeyHex)), "encrypt_type": 1},
		"aeskey":   u.KeyHex,
		"mid_size": u.CipherSize,
	}}
	msg := map[string]any{
		"from_user_id":  "",
		"to_user_id":    peer,
		"client_id":     clientID,
		"message_type":  typeBot,
		"message_state": stateFinish,
		"item_list":     []any{item},
		"context_token": contextToken,
	}
	out, err := c.call(ctx, http.MethodPost, "ilink/bot/sendmessage", map[string]any{"msg": msg, "base_info": c.baseInfo()}, true, apiTimeout, nil)
	if err != nil {
		return "", err
	}
	if err := refusal(out, "the picture"); err != nil {
		return "", err
	}
	return responseMessageID(out["message_id"]), nil
}
