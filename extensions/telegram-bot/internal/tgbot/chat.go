package tgbot

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	imagesMost  = 4
	filesMost   = 4
	maxSeen     = 200
	strangerGap = 10 * time.Minute
	typingEvery = 4 * time.Second
	typingFor   = 30 * time.Second
)

func (b *Bot) handleMessage(ctx context.Context, c *Client, creds credentials, m Message) {
	if m.Chat.Type != "private" || m.From == nil {
		return
	}
	b.mu.Lock()
	if slices.Contains(b.st.Seen, m.MessageID) {
		b.mu.Unlock()
		return
	}
	b.st.Seen = append(b.st.Seen, m.MessageID)
	if len(b.st.Seen) > maxSeen {
		b.st.Seen = b.st.Seen[len(b.st.Seen)-maxSeen:]
	}
	b.status.LastEvent = b.opt.Now()
	b.saveLocked(ctx)
	b.mu.Unlock()

	if m.From.ID != creds.OwnerID || m.Chat.ID != creds.ChatID {
		b.stranger(ctx, c, m)
		return
	}

	msg := b.compose(ctx, c, m)
	if emptyMessage(msg) {
		return
	}
	if b.opt.Hub == nil {
		log.Printf("telegram-bot: no hub to deliver message")
		return
	}
	if err := b.opt.Hub.Receive(ctx, msg); err != nil {
		log.Printf("telegram-bot: deliver to hub: %v", err)
		_, _ = c.SendMessage(ctx, m.Chat.ID, t(b.lang(), "chat.hub_error"), m.MessageID)
	}
}

func (b *Bot) stranger(ctx context.Context, c *Client, m Message) {
	b.mu.Lock()
	now := b.opt.Now()
	last, ok := b.replied[m.From.ID]
	if ok && now.Sub(last) < strangerGap {
		b.mu.Unlock()
		return
	}
	b.replied[m.From.ID] = now
	for who, at := range b.replied {
		if now.Sub(at) >= strangerGap {
			delete(b.replied, who)
		}
	}
	b.mu.Unlock()
	_, _ = c.SendMessage(ctx, m.Chat.ID, t(b.lang(), "chat.stranger"), m.MessageID)
}

func (b *Bot) compose(ctx context.Context, c *Client, m Message) wire.ChannelMessage {
	owner := true
	text := strings.TrimSpace(m.Text)
	if text == "" {
		text = strings.TrimSpace(m.Caption)
	}
	out := wire.ChannelMessage{
		ID:       strconv.FormatInt(m.MessageID, 10),
		Text:     text,
		ChatID:   strconv.FormatInt(m.Chat.ID, 10),
		ChatKind: "direct",
		Owner:    &owner,
	}
	if m.ReplyToMessage != nil {
		out.ReplyTo = strconv.FormatInt(m.ReplyToMessage.MessageID, 10)
		q := strings.TrimSpace(m.ReplyToMessage.Text)
		if q == "" {
			q = strings.TrimSpace(m.ReplyToMessage.Caption)
		}
		out.Quote = q
	}
	if len(m.Photo) > 0 && len(out.Images) < imagesMost {
		best := pickBestPhoto(m.Photo, maxImageBytes)
		if best != nil {
			if blob, ok := b.downloadBlob(ctx, c, best.FileID, "photo.jpg", "image/jpeg", maxImageBytes); ok {
				out.Images = append(out.Images, blob)
			} else {
				out.Unsupported = append(out.Unsupported, "photo")
			}
		} else {
			out.Unsupported = append(out.Unsupported, "photo")
		}
	}
	if m.Document != nil && len(out.Files) < filesMost {
		name := m.Document.FileName
		if name == "" {
			name = "attachment"
		}
		if strings.HasPrefix(m.Document.MimeType, "image/") && len(out.Images) < imagesMost &&
			(m.Document.FileSize == 0 || m.Document.FileSize <= maxImageBytes) {
			if blob, ok := b.downloadBlob(ctx, c, m.Document.FileID, name, m.Document.MimeType, maxImageBytes); ok {
				out.Images = append(out.Images, blob)
			} else {
				out.Unsupported = append(out.Unsupported, "image")
			}
		} else if m.Document.FileSize == 0 || m.Document.FileSize <= maxFileBytes {
			if blob, ok := b.downloadBlob(ctx, c, m.Document.FileID, name, m.Document.MimeType, maxFileBytes); ok {
				out.Files = append(out.Files, blob)
			} else {
				out.Unsupported = append(out.Unsupported, "file")
			}
		} else {
			out.Unsupported = append(out.Unsupported, "file")
		}
	}
	if len(m.Voice) > 0 {
		out.Unsupported = append(out.Unsupported, "voice")
	}
	if len(m.Audio) > 0 {
		out.Unsupported = append(out.Unsupported, "audio")
	}
	if len(m.Video) > 0 || len(m.VideoNote) > 0 {
		out.Unsupported = append(out.Unsupported, "video")
	}
	if len(m.Sticker) > 0 {
		out.Unsupported = append(out.Unsupported, "sticker")
	}
	if len(m.Location) > 0 {
		out.Unsupported = append(out.Unsupported, "location")
	}
	if len(m.Contact) > 0 {
		out.Unsupported = append(out.Unsupported, "contact")
	}
	return out
}

func pickBestPhoto(sizes []PhotoSize, maxBytes int64) *PhotoSize {
	var best *PhotoSize
	for i := range sizes {
		s := &sizes[i]
		if s.FileSize > 0 && s.FileSize > maxBytes {
			continue
		}
		if best == nil || s.Width*s.Height >= best.Width*best.Height {
			best = s
		}
	}
	return best
}

func (b *Bot) downloadBlob(ctx context.Context, c *Client, fileID, fallbackName, fallbackMime string, maxBytes int64) (wire.ChannelBlob, bool) {
	f, err := c.GetFile(ctx, fileID)
	if err != nil {
		log.Printf("telegram-bot: getFile failed: %v", err)
		return wire.ChannelBlob{}, false
	}
	if f.FileSize > 0 && f.FileSize > maxBytes {
		return wire.ChannelBlob{}, false
	}
	data, err := c.DownloadFile(ctx, f.FilePath, maxBytes)
	if err != nil {
		log.Printf("telegram-bot: download file failed: %v", err)
		return wire.ChannelBlob{}, false
	}
	name := fallbackName
	if name == "" || name == "photo.jpg" {
		if base := path.Base(f.FilePath); base != "" && base != "." && base != "/" {
			name = base
		}
	}
	mime := fallbackMime
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	return wire.ChannelBlob{
		Name: name,
		Mime: mime,
		Data: base64.StdEncoding.EncodeToString(data),
	}, true
}

func emptyMessage(m wire.ChannelMessage) bool {
	return strings.TrimSpace(m.Text) == "" && len(m.Images) == 0 && len(m.Files) == 0 &&
		m.Quote == "" && m.Forward == "" && len(m.Unsupported) == 0
}

func (b *Bot) sendGeneration() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sendGen
}

// Send delivers text and optional images to the bound owner's private Telegram chat.
func (b *Bot) Send(ctx context.Context, p wire.ChannelSendParams) (wire.ChannelSendResult, error) {
	b.mu.Lock()
	creds := b.st.Creds
	b.mu.Unlock()
	if creds == nil || creds.Token == "" || creds.ChatID == 0 {
		return wire.ChannelSendResult{}, errCannotSend
	}
	c, err := b.ensureClient(creds)
	if err != nil {
		return wire.ChannelSendResult{}, errCannotSend
	}
	var replyTo int64
	if p.ReplyTo != "" {
		if parsed, perr := strconv.ParseInt(p.ReplyTo, 10, 64); perr == nil && parsed > 0 {
			replyTo = parsed
		}
	}
	var lastMsgID int64
	for i, part := range splitText(p.Text, MaxText) {
		r := int64(0)
		if i == 0 {
			r = replyTo
		}
		id, serr := c.SendMessage(ctx, creds.ChatID, part, r)
		if serr != nil {
			log.Printf("telegram-bot: send message failed: %v", serr)
			return wire.ChannelSendResult{}, errors.New(t(b.lang(), "chat.send_failed"))
		}
		lastMsgID = id
	}
	failedImages := 0
	for _, img := range p.Images {
		raw, derr := base64.StdEncoding.DecodeString(img.Data)
		if derr != nil || len(raw) == 0 {
			failedImages++
			continue
		}
		id, serr := c.SendPhoto(ctx, creds.ChatID, img.Name, img.Mime, raw, replyTo)
		if serr != nil {
			log.Printf("telegram-bot: send photo failed: %v", serr)
			failedImages++
			continue
		}
		lastMsgID = id
		replyTo = 0
	}
	if failedImages > 0 {
		_, _ = c.SendMessage(ctx, creds.ChatID, t(b.lang(), "chat.images_not_sent", "count", strconv.Itoa(failedImages)), 0)
	}
	if lastMsgID > 0 {
		return wire.ChannelSendResult{MessageID: strconv.FormatInt(lastMsgID, 10)}, nil
	}
	return wire.ChannelSendResult{}, nil
}

func splitText(s string, maxRunes int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return []string{s}
	}
	var parts []string
	for len(s) > 0 {
		if utf8.RuneCountInString(s) <= maxRunes {
			parts = append(parts, s)
			break
		}
		cut := byteIndexAtRune(s, maxRunes)
		if nl := strings.LastIndexByte(s[:cut], '\n'); nl > cut/2 {
			cut = nl + 1
		}
		piece := strings.TrimSpace(s[:cut])
		if piece != "" {
			parts = append(parts, piece)
		}
		s = strings.TrimSpace(s[cut:])
	}
	return parts
}

func byteIndexAtRune(s string, n int) int {
	i := 0
	for count := 0; count < n && i < len(s); count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return i
}

type typingLoop struct {
	stop chan struct{}
	done chan struct{}
}

// Typing shows or stops the Telegram "typing..." chat action while a turn is running.
func (b *Bot) Typing(ctx context.Context, on bool) error {
	if !on {
		b.stopTyping()
		return nil
	}
	b.mu.Lock()
	creds := b.st.Creds
	already := b.typing != nil
	b.mu.Unlock()
	if creds == nil || creds.Token == "" || creds.ChatID == 0 || already {
		return nil
	}
	c, err := b.ensureClient(creds)
	if err != nil {
		return nil
	}
	tl := &typingLoop{stop: make(chan struct{}), done: make(chan struct{})}
	b.mu.Lock()
	if b.typing != nil {
		b.mu.Unlock()
		return nil
	}
	b.typing = tl
	chatID := creds.ChatID
	b.mu.Unlock()

	_ = c.SendChatAction(ctx, chatID, "typing")
	go func() {
		defer close(tl.done)
		tk := time.NewTicker(typingEvery)
		defer tk.Stop()
		deadline := time.NewTimer(typingFor)
		defer deadline.Stop()
		for {
			select {
			case <-tl.stop:
				return
			case <-deadline.C:
				b.mu.Lock()
				if b.typing == tl {
					b.typing = nil
				}
				b.mu.Unlock()
				return
			case <-tk.C:
				cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = c.SendChatAction(cctx, chatID, "typing")
				cancel()
			}
		}
	}()
	return nil
}

func (b *Bot) stopTyping() {
	b.mu.Lock()
	tl := b.typing
	b.typing = nil
	b.mu.Unlock()
	if tl != nil {
		close(tl.stop)
		<-tl.done
	}
}
