package dingtalk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona-extensions/extensions/dingtalk/internal/dingtalk/api"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	// imagesMost is how many pictures one message brings the hub, imageMost and fileMost how large each may be
	// (docs/extensions/channels.md "Protocol" in Persona).
	imagesMost = 4
	imageMost  = 3 << 20
	fileMost   = 50 << 20
	// blobsMost is how many bytes of pictures and files one message brings in all: the hub's own total
	// (docs/extensions/channels.md "Protocol"). It also keeps the base64 channel.receive frame (4/3 of it, ≈ 53 MiB)
	// under the extension protocol's 64 MiB line, which a single 50 MB file would overrun.
	blobsMost = 40 << 20
	// frameMost is the most a channel.receive may encode to: the protocol reader's 64 MiB line, less room for the
	// envelope. A message over it loses its blobs, last first, rather than the connection.
	frameMost   = 63 << 20
	maxSeen     = 300
	strangerGap = 10 * time.Minute
	// titleMost is how long the title DingTalk shows in the chat list and notifications may be.
	titleMost = 30
)

// sendRetryWait is the first wait before sending a piece again (then twice, three times as long); tests shorten it.
var sendRetryWait = time.Second

// handle takes one message to the robot and says whether it is settled (its Stream frame may be answered). Groups are
// never handled; in a one-to-one chat the owner's message goes to the hub, a pairing code pairs, and anyone else hears
// whose robot it is. The owner's message counts as seen only once the hub took it: when the hub fails, it is neither
// marked seen nor settled, so DingTalk delivers it again.
func (b *Bot) handle(ctx context.Context, c *api.Client, m api.Message) bool {
	if m.ConversationType != api.ConversationSingle {
		return true
	}
	b.mu.Lock()
	if slices.Contains(b.st.Seen, m.ID) {
		b.mu.Unlock()
		return true
	}
	o := b.st.Owner
	if o != nil && m.SenderStaffID != "" && m.SenderStaffID == o.StaffID {
		b.mu.Unlock()
		if !b.forward(ctx, c, m) {
			return false
		}
		b.mu.Lock()
		b.markSeenLocked(m.ID)
		b.last = b.opt.Now()
		if o := b.st.Owner; o != nil && m.SenderNick != "" && m.SenderNick != o.Name {
			o.Name = m.SenderNick
		}
		b.saveLocked(ctx)
		b.mu.Unlock()
		return true
	}
	b.markSeenLocked(m.ID)
	var reply string
	handled := false
	if m.SenderStaffID != "" {
		reply, handled = b.tryPairLocked(ctx, m.SenderStaffID, m.SenderNick, m.PlainText())
	}
	b.saveLocked(ctx)
	b.mu.Unlock()
	if handled {
		b.reply(ctx, c, m, reply)
	} else {
		b.stranger(ctx, c, m, o == nil)
	}
	return true
}

// markSeenLocked records id among the latest messages handled; b.mu is held.
func (b *Bot) markSeenLocked(id string) {
	b.st.Seen = append(b.st.Seen, id)
	if len(b.st.Seen) > maxSeen {
		b.st.Seen = b.st.Seen[len(b.st.Seen)-maxSeen:]
	}
}

// forward hands the owner's message to the hub and says whether the hub took it. When it did not, the owner hears so
// once per message (DingTalk delivers it again, and it is tried again then).
func (b *Bot) forward(ctx context.Context, c *api.Client, m api.Message) bool {
	msg := b.compose(ctx, c, m)
	if empty(msg) || b.opt.Hub == nil {
		return true
	}
	if err := b.opt.Hub.Receive(ctx, msg); err != nil {
		log.Printf("dingtalk: handing a message to the hub: %v", err)
		b.mu.Lock()
		told := b.hubErrorTold[m.ID]
		if !told {
			if len(b.hubErrorTold) >= maxSeen {
				clear(b.hubErrorTold)
			}
			b.hubErrorTold[m.ID] = true
		}
		b.mu.Unlock()
		if !told {
			b.reply(ctx, c, m, t(b.lang(), "chat.hub_error"))
		}
		return false
	}
	return true
}

// stranger: the robot only serves the person who paired it; anyone else hears so (or how to pair, before anyone
// has), at most once in ten minutes.
func (b *Bot) stranger(ctx context.Context, c *api.Client, m api.Message, unpaired bool) {
	who := m.SenderStaffID
	if who == "" {
		who = "conversation:" + m.ConversationID
	}
	b.mu.Lock()
	last, ok := b.replied[who]
	now := b.opt.Now()
	if ok && now.Sub(last) < strangerGap {
		b.mu.Unlock()
		return
	}
	b.replied[who] = now
	for w, at := range b.replied {
		if now.Sub(at) >= strangerGap {
			delete(b.replied, w)
		}
	}
	b.mu.Unlock()
	key := "chat.stranger"
	if unpaired {
		key = "chat.not_paired"
	}
	b.reply(ctx, c, m, t(b.lang(), key))
}

// reply answers in the chat m came from, through its session webhook. A failure is logged (never the text).
func (b *Bot) reply(ctx context.Context, c *api.Client, m api.Message, text string) {
	if m.SessionWebhook == "" || m.SessionWebhookExpiredTime > 0 && b.opt.Now().After(time.UnixMilli(m.SessionWebhookExpiredTime)) {
		return
	}
	if err := c.Reply(ctx, m.SessionWebhook, text); err != nil {
		log.Printf("dingtalk: replying in the chat: %s", api.Redact(err))
	}
}

// Send is the hub's channel.send: Markdown to the owner, in their chat with the robot. Before anyone paired there is
// nobody to send to: errCannotSend.
func (b *Bot) Send(ctx context.Context, p wire.ChannelSendParams) (wire.ChannelSendResult, error) {
	b.mu.Lock()
	c, o := b.client, b.st.Owner
	b.mu.Unlock()
	if c == nil || o == nil {
		return wire.ChannelSendResult{}, errCannotSend
	}
	pieces := Chunk(strings.TrimSpace(p.Text), MaxText)
	if len(pieces) == 0 {
		return wire.ChannelSendResult{}, errNothingToSend // never reported as delivered
	}
	for i, part := range pieces {
		var err error
		for attempt := range 3 {
			// sent again only when DingTalk certainly did not take it: a resend after an unknown outcome is a
			// duplicate in the owner's chat (DingTalk keeps no idempotency key)
			if err = c.SendMarkdown(ctx, o.StaffID, Title(part), part); err == nil || !api.NotSent(err) {
				break
			}
			if !sleep(ctx, time.Duration(attempt+1)*sendRetryWait) {
				return wire.ChannelSendResult{}, keep(i, ctx.Err())
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, api.ErrNotReachable) && i == 0:
			return wire.ChannelSendResult{}, errCannotSend
		case api.NotSent(err) || api.Refused(err):
			log.Printf("dingtalk: sending a message: %s", api.Redact(err))
			return wire.ChannelSendResult{}, keep(i, sendFailed{b.lang()})
		default:
			log.Printf("dingtalk: sending a message, outcome unknown: %s", api.Redact(err))
			return wire.ChannelSendResult{}, kept{sendUncertain{b.lang()}}
		}
	}
	return wire.ChannelSendResult{}, nil
}

// errNothingToSend: channel.send with no text; nothing was delivered.
var errNothingToSend = errors.New("nothing to send: the message has no text")

// sendFailed: DingTalk did not take the message (logged by the hub, which sends it again with the same ref).
type sendFailed struct{ lang string }

func (e sendFailed) Error() string { return t(e.lang, "chat.send_failed") }

// sendUncertain: the message went out but DingTalk's answer did not come back; it may have been delivered.
type sendUncertain struct{ lang string }

func (e sendUncertain) Error() string { return t(e.lang, "chat.send_uncertain") }

// kept is a send failure whose ref must not be sent again: part of it may already be in the owner's chat.
type kept struct{ error }

func (k kept) Unwrap() error { return k.error }

// keep is err for a send that failed at piece i: kept when earlier pieces were delivered.
func keep(i int, err error) error {
	if i > 0 {
		return kept{err}
	}
	return err
}

// empty says whether m has nothing to hand on.
func empty(m wire.ChannelMessage) bool {
	return strings.TrimSpace(m.Text) == "" && len(m.Images) == 0 && len(m.Files) == 0 && m.Quote == "" && m.Forward == "" &&
		len(m.Unsupported) == 0
}

// compose is the owner's message as the hub takes it: the text (a voice message's transcript), the pictures and the
// files; what cannot be brought is named for the assistant.
func (b *Bot) compose(ctx context.Context, c *api.Client, m api.Message) wire.ChannelMessage {
	out := wire.ChannelMessage{ID: m.ID}
	var text strings.Builder
	left := int64(blobsMost) // bytes of pictures and files still allowed
	for _, p := range m.Parts() {
		switch p.Kind {
		case api.KindText:
			text.WriteString(p.Text)
		case api.KindPicture:
			if len(out.Images) >= imagesMost {
				out.Unsupported = append(out.Unsupported, "更多的图片") // i18n-ignore: what the assistant reads
				continue
			}
			most := min(int64(imageMost), left)
			if most <= 0 {
				out.Unsupported = append(out.Unsupported, tooMuch) // i18n-ignore: what the assistant reads
				continue
			}
			data, err := download(ctx, c, p.DownloadCode, most)
			switch {
			case errors.Is(err, api.ErrTooLarge) && most < imageMost:
				out.Unsupported = append(out.Unsupported, tooMuch)
			case errors.Is(err, api.ErrTooLarge):
				out.Unsupported = append(out.Unsupported, "一张太大的图片") // i18n-ignore: what the assistant reads
			case err != nil:
				log.Printf("dingtalk: downloading a picture: %s", api.Redact(err))
				out.Unsupported = append(out.Unsupported, "一张没下载下来的图片") // i18n-ignore: what the assistant reads
			default:
				left -= int64(len(data))
				out.Images = append(out.Images, wire.ChannelBlob{Mime: http.DetectContentType(data), Data: base64.StdEncoding.EncodeToString(data)})
			}
		case api.KindAudio:
			if s := strings.TrimSpace(p.Text); s != "" {
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(s)
			} else {
				out.Unsupported = append(out.Unsupported, "一段没有转写的语音") // i18n-ignore: what the assistant reads
			}
		case api.KindFile:
			name := p.FileName
			most := min(int64(fileMost), left)
			if most <= 0 {
				out.Unsupported = append(out.Unsupported, fileLabel(name)+"（"+tooMuch+"）") // i18n-ignore: what the assistant reads
				continue
			}
			data, err := download(ctx, c, p.DownloadCode, most)
			switch {
			case errors.Is(err, api.ErrTooLarge) && most < fileMost:
				out.Unsupported = append(out.Unsupported, fileLabel(name)+"（"+tooMuch+"）") // i18n-ignore: what the assistant reads
			case errors.Is(err, api.ErrTooLarge):
				out.Unsupported = append(out.Unsupported, fileLabel(name)+"（超过 50 MB）") // i18n-ignore: what the assistant reads
			case err != nil:
				log.Printf("dingtalk: downloading a file: %s", api.Redact(err))
				out.Unsupported = append(out.Unsupported, "没下载下来的"+fileLabel(name)) // i18n-ignore: what the assistant reads
			default:
				if name == "" {
					name = "file"
				}
				left -= int64(len(data))
				out.Files = append(out.Files, wire.ChannelBlob{Name: name, Mime: fileMime(name, data), Data: base64.StdEncoding.EncodeToString(data)})
			}
		case api.KindVideo:
			out.Unsupported = append(out.Unsupported, "一段视频") // i18n-ignore: what the assistant reads
		default:
			out.Unsupported = append(out.Unsupported, "一条暂不支持的钉钉消息") // i18n-ignore: what the assistant reads
		}
	}
	out.Text = strings.TrimSpace(text.String())
	fitFrame(&out)
	out.Unsupported = slices.Compact(out.Unsupported)
	return out
}

// tooMuch names what did not fit in a message's total for pictures and files.
const tooMuch = "附件合计超过 40 MB，放不下" // i18n-ignore: what the assistant reads

// fitFrame drops m's files, then pictures, last first, until channel.receive's frame fits under frameMost (blobsMost
// already keeps it there; this holds for a long text too).
func fitFrame(m *wire.ChannelMessage) {
	for frameSize(*m) > frameMost && len(m.Files)+len(m.Images) > 0 {
		if n := len(m.Files); n > 0 {
			m.Unsupported = append(m.Unsupported, fileLabel(m.Files[n-1].Name)+"（"+tooMuch+"）") // i18n-ignore: what the assistant reads
			m.Files = m.Files[:n-1]
		} else {
			m.Unsupported = append(m.Unsupported, tooMuch)
			m.Images = m.Images[:len(m.Images)-1]
		}
	}
}

// frameSize is how large channel.receive with m encodes to.
func frameSize(m wire.ChannelMessage) int {
	raw, err := json.Marshal(wire.ChannelReceiveParams{Contribution: ID, Message: m})
	if err != nil {
		return 0
	}
	return len(raw)
}

func download(ctx context.Context, c *api.Client, code string, most int64) ([]byte, error) {
	if code == "" {
		return nil, errors.New("no download code")
	}
	return c.Download(ctx, code, most)
}

// fileMime is a file's type: from its bytes, else from its name's extension (an old .doc reads as plain bytes).
func fileMime(name string, data []byte) string {
	if m := http.DetectContentType(data); m != "application/octet-stream" {
		return m
	}
	if m := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); m != "" {
		return m
	}
	return "application/octet-stream"
}

func fileLabel(name string) string {
	if name == "" {
		return "一个文件" // i18n-ignore: what the assistant reads
	}
	return "文件「" + name + "」" // i18n-ignore: what the assistant reads
}

// Chunk cuts text into pieces of at most most characters, at a line break when there is one in the second half of
// a piece. The hub already cuts at MaxText; this only guards what it did not.
func Chunk(text string, most int) []string {
	if text == "" {
		return nil
	}
	var out []string
	for utf8.RuneCountInString(text) > most {
		r := []rune(text)
		cut := most
		for i := most - 1; i >= most/2; i-- {
			if r[i] == '\n' {
				cut = i + 1
				break
			}
		}
		// only newlines are trimmed at a cut, and a piece of nothing but newlines is skipped: every other
		// character is kept, in order, and no empty piece is sent
		if piece := strings.TrimRight(string(r[:cut]), "\n"); piece != "" {
			out = append(out, piece)
		}
		text = strings.TrimLeft(string(r[cut:]), "\n")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// Title is what DingTalk shows for a Markdown message in the chat list and its notification: the first line with
// text, its Markdown marks taken off, cut to titleMost characters.
func Title(markdown string) string {
	for _, line := range strings.Split(markdown, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "```") {
			continue
		}
		s = strings.TrimLeft(s, "#>-*+ \t")
		s = strings.NewReplacer("**", "", "__", "", "`", "", "~~", "").Replace(s)
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if r := []rune(s); len(r) > titleMost {
			s = string(r[:titleMost-1]) + "…"
		}
		return s
	}
	return "Persona"
}
