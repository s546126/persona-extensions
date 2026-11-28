package wechat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/wechat/internal/wechat/ilink"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	// imagesMost is how many pictures one message brings the hub (docs/extensions/channels.md "Protocol").
	imagesMost = 4
	// messageMediaMost is the hub's raw image + file budget for one message, before Base64 reaches the process RPC.
	messageMediaMost = ilink.MaxFile
	// typingEvery is how often "typing…" is shown again while the hub keeps asking for it.
	typingEvery = 5 * time.Second
	maxSeen     = 200
	maxReplied  = 1024
	strangerGap = 10 * time.Minute
	// typingFor: "typing…" is let go when the hub has not asked for it again in this long (it asks every 5 s while a
	// turn goes on): a hub that went away does not leave it on.
	typingFor = 30 * time.Second
)

// handle takes one message the bot got: someone else's gets one line saying whose bot it is; the owner's goes to the
// hub, which knows what it is (a command, an answer to what was asked, a message for the conversation).
func (b *Bot) handle(ctx context.Context, c *ilink.Client, creds ilink.Credentials, m ilink.Message) {
	owner := m.From == creds.UserID
	if !owner {
		if !b.isBound(creds.Token) {
			return
		}
		b.stranger(ctx, c, m)
		return
	}
	b.mu.Lock()
	if b.st.Creds == nil || b.st.Creds.Token != creds.Token {
		b.mu.Unlock()
		return
	}
	if slices.Contains(b.st.Seen, m.ID) {
		b.mu.Unlock()
		return
	}
	b.st.Seen = append(b.st.Seen, m.ID)
	if len(b.st.Seen) > maxSeen {
		b.st.Seen = b.st.Seen[len(b.st.Seen)-maxSeen:]
	}
	b.status.LastEvent = b.opt.Now()
	b.st.Context = m.ContextToken
	b.saveLocked(ctx)
	b.mu.Unlock()
	msg := b.compose(ctx, c, creds.UserID, m)
	if empty(msg) {
		return
	}
	if b.opt.Hub == nil {
		log.Printf("wechat: no hub to hand the message to")
		return
	}
	if err := b.opt.Hub.Receive(ctx, msg); err != nil {
		log.Printf("wechat: handing a message to the hub: %v", err)
		b.send(ctx, c, m.From, m.ContextToken, m.ID, "", t(b.lang(), "chat.hub_error"))
	}
}

// stranger: the bot only serves the WeChat that bound it; anyone else hears so, at most once in ten minutes.
func (b *Bot) stranger(ctx context.Context, c *ilink.Client, m ilink.Message) {
	b.mu.Lock()
	last, ok := b.replied[m.From]
	now := b.opt.Now()
	if ok && now.Sub(last) < strangerGap {
		b.mu.Unlock()
		return
	}
	for who, at := range b.replied {
		if now.Sub(at) >= strangerGap {
			delete(b.replied, who)
		}
	}
	if len(b.replied) >= maxReplied {
		var oldestKey string
		var oldestAt time.Time
		for who, at := range b.replied {
			if oldestKey == "" || at.Before(oldestAt) {
				oldestKey, oldestAt = who, at
			}
		}
		if oldestKey != "" {
			delete(b.replied, oldestKey)
		}
	}
	b.replied[m.From] = now
	b.mu.Unlock()
	b.send(ctx, c, m.From, m.ContextToken, m.ID, "", t(b.lang(), "chat.stranger"))
}

// send sends text to peer as the bot: WeChat's subset of Markdown, in pieces it takes. ref, when given, is the hub's id
// for the message, which each piece's client_id comes from (sendIDs); without one, each piece gets a random id.
func (b *Bot) send(ctx context.Context, c *ilink.Client, peer, contextToken, replyTo, ref, text string) bool {
	if c == nil || contextToken == "" {
		log.Printf("wechat: nothing to send with (no client or context token)")
		return false
	}
	parts := splitText(text)
	ids := newSendIDs(ref, peer, 0, parts, nil)
	for i, part := range parts {
		if !b.sendPiece(ctx, c, peer, contextToken, replyTo, ref, partOf(i, len(parts)), ids.parts[i], part) {
			return false
		}
	}
	return true
}

// splitText is the pieces of text that WeChat takes, one message each.
func splitText(text string) []string {
	return ilink.Chunk(ilink.FilterMarkdown(strings.TrimSpace(text)), 0)
}

// partOf is how the log names text piece i of n.
func partOf(i, n int) string { return fmt.Sprintf("part=%d/%d", i+1, n) }

// sendTries is how many times a piece or a picture is tried while its failure may pass (ilink.Retryable).
const sendTries = 3

// sendRetryWait is the wait before the second try, and twice it before the third; tests shorten it.
var sendRetryWait = time.Second

// retryWait waits between tries; tests count it.
var retryWait = sleep

// retry calls try until it succeeds, fails in a way that cannot pass, or was called sendTries times, waiting
// sendRetryWait and then twice that between calls; no wait follows the last call, so its error is what retry answers.
// Cancelled while waiting, retry answers the cancellation.
func retry(ctx context.Context, try func() error) error {
	for attempt := 1; ; attempt++ {
		err := try()
		if err == nil || !ilink.Retryable(err) || attempt == sendTries {
			return err
		}
		if !retryWait(ctx, time.Duration(attempt)*sendRetryWait) {
			return ctx.Err()
		}
	}
}

// sendPiece sends one text piece with id as its client_id, trying a failure that may pass again. Its final result is
// logged with the inbound message, ref, what (its place: part=i/n, or note) and duration, never the text; a piece WeChat
// confirmed goes into the quote index.
func (b *Bot) sendPiece(ctx context.Context, c *ilink.Client, peer, contextToken, replyTo, ref, what, id, text string) bool {
	started := time.Now()
	var serverID string
	err := retry(ctx, func() (err error) {
		serverID, err = c.SendTextWithID(ctx, peer, contextToken, id, text)
		return err
	})
	took := time.Since(started)
	if err != nil {
		log.Printf("wechat: send reply_to=%q ref=%q %s result=failure duration=%s error_type=%s error=%v",
			replyTo, ref, what, took, ilink.ErrorType(err), err)
		return false
	}
	log.Printf("wechat: send reply_to=%q ref=%q %s result=success duration=%s", replyTo, ref, what, took)
	b.rememberOutbound(peer, serverID, text, nil)
	return true
}

// rememberOutbound keeps what the bot sent the owner in the quote index by its server message id (text, or a
// picture's summary), so that the owner quoting it is restored like a quote of their own message.
func (b *Bot) rememberOutbound(peer, serverID, text string, media []quoteMedia) {
	if b.quotes == nil || serverID == "" {
		return
	}
	b.mu.Lock()
	owner := b.st.Creds != nil && b.st.Creds.UserID == peer
	b.mu.Unlock()
	if owner {
		b.quotes.put(quoteCacheID(peer, serverID), text, media)
	}
}

// Send is sendIn for the WeChat bound now.
func (b *Bot) Send(ctx context.Context, p wire.ChannelSendParams) (wire.ChannelSendResult, error) {
	return b.sendIn(ctx, b.sendGeneration(), p)
}

// sendGeneration is the bot's sending generation now (Bot.sendGen): the binding a channel.send comes in for.
func (b *Bot) sendGeneration() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sendGen
}

// sendIn is the hub's channel.send that came in during sending generation gen (Bot.sendGen): text to the owner, in the
// chat of their latest message, then its pictures. Before the owner has written to the bot there is no context token,
// and nothing can be sent: errCannotSend. A send that starts after the binding changed (it waited for an earlier send of
// its ref) was for a WeChat no longer bound, and sends nothing: errRebound.
//
// A picture that cannot be sent (not a picture, too large, or refused by WeChat in a way that does not pass) is skipped
// and the pictures after it go on; a last text then tells the owner how many were not sent. A failure that may pass
// stops the send (sendFailed), and so does sendDeadline: the hub sends the same ref again, and what WeChat confirmed of
// it is remembered by ref (sendProgress), so the next send of the ref goes on from the piece or picture that failed.
func (b *Bot) sendIn(ctx context.Context, gen uint64, p wire.ChannelSendParams) (wire.ChannelSendResult, error) {
	parts := splitText(p.Text)
	sum := ""
	if p.Ref != "" {
		sum = messageSum(parts, p.Images)
	}
	b.mu.Lock()
	if gen != b.sendGen {
		b.mu.Unlock()
		log.Printf("wechat: send reply_to=%q ref=%q not sent: the WeChat it came in for is no longer bound", p.ReplyTo, p.Ref)
		return wire.ChannelSendResult{}, errRebound
	}
	c, creds, contextToken := b.client, b.st.Creds, b.st.Context
	round := b.roundLocked(p.Ref, sum)
	b.mu.Unlock()
	if creds == nil || c == nil || contextToken == "" {
		return wire.ChannelSendResult{}, errCannotSend
	}
	ctx, cancel := context.WithTimeout(ctx, sendDeadline)
	defer cancel()
	peer := creds.UserID
	ids := sendIDsOf(p.Ref, peer, round, sum, len(parts), len(p.Images))
	pr := b.resume(p.Ref, gen, ids)
	if textDone, imagesDone := countIn(pr.done, ids.parts), countIn(pr.done, ids.images); textDone > 0 || imagesDone > 0 {
		log.Printf("wechat: send reply_to=%q ref=%q resume text=%d/%d image=%d/%d",
			p.ReplyTo, p.Ref, textDone, len(parts), imagesDone, len(p.Images))
	}
	failed := func() (wire.ChannelSendResult, error) {
		b.keep(p.Ref, gen, pr)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			log.Printf("wechat: send reply_to=%q ref=%q stopped: out of time (%s at most); the hub's retry goes on from here",
				p.ReplyTo, p.Ref, sendDeadline)
		}
		return wire.ChannelSendResult{}, sendFailed{b.lang()}
	}
	for i, part := range parts {
		id := ids.parts[i]
		if pr.done[id] {
			continue
		}
		if !b.sendPiece(ctx, c, peer, contextToken, p.ReplyTo, p.Ref, partOf(i, len(parts)), id, part) {
			return failed()
		}
		pr.done[id] = true
	}
	for i, img := range p.Images {
		id := ids.images[i]
		if pr.done[id] {
			continue
		}
		up := pr.uploaded(id)
		switch b.sendImage(ctx, c, peer, contextToken, p.ReplyTo, p.Ref, id, i, len(p.Images), img, &up) {
		case imageFailed:
			pr.upload, pr.uploadOf = up, id
			return failed()
		case imageSkipped:
			pr.skipped[id] = true
		}
		pr.done[id] = true
	}
	if n := countIn(pr.skipped, ids.images); n > 0 {
		note := t(b.lang(), "chat.images_not_sent", "count", strconv.Itoa(n))
		if !b.sendPiece(ctx, c, peer, contextToken, p.ReplyTo, p.Ref, "note", ids.note(n), note) {
			return failed()
		}
	}
	b.forget(p.Ref)
	return wire.ChannelSendResult{}, nil
}

// sendDeadline is how long one channel.send may take, counted from the call (program.channel: waiting for an earlier
// send of the same ref counts too). The hub waits 2 minutes for a call to a connector's extension, and restarts the
// extension after three calls in a row time out (Persona's server/internal/extmgr/extensions.go, CallTimeout, and
// server/internal/extension/host/host.go, StrikesToKill). A send still going at sendDeadline stops as a failure that
// may pass, before the hub gives up on the call; what it got is kept for the hub's retry. Tests shorten it.
var sendDeadline = 100 * time.Second

// sendIDs are the client_ids of one send's text pieces and pictures. With a ref, each comes from the ref, the WeChat it
// goes to (peer), the send's round (roundLocked), sum (a digest of all its text and pictures) and the piece's kind and
// place: the hub's retry of the same message has the same ids, so iLink may keep a repeat once and the progress kept
// by ref knows them. Other content under the same ref is a new round with new ids for every piece, and so is content
// that comes back after other content (A, B, then A again): it is sent again from the start, and iLink takes none of
// it for a repeat; nor does it take the same message to a WeChat bound later for one sent before. (The hub fetches an
// answer's pictures again each time it sends the answer, and a picture it could not fetch stays a link in the text:
// its retry of an answer may differ.) Without a ref the ids are random.
type sendIDs struct {
	ref, peer, sum string
	round          int
	parts, images  []string
}

// newSendIDs are the ids of parts and images sent to peer under ref in round.
func newSendIDs(ref, peer string, round int, parts []string, images []wire.ChannelBlob) sendIDs {
	sum := ""
	if ref != "" {
		sum = messageSum(parts, images)
	}
	return sendIDsOf(ref, peer, round, sum, len(parts), len(images))
}

// sendIDsOf are the ids of n text pieces and m pictures whose digest is sum (messageSum).
func sendIDsOf(ref, peer string, round int, sum string, n, m int) sendIDs {
	ids := sendIDs{ref: ref, peer: peer, sum: sum, round: round}
	for i := range n {
		ids.parts = append(ids.parts, ids.id("text", i))
	}
	for i := range m {
		ids.images = append(ids.images, ids.id("image", i))
	}
	return ids
}

// roundsMost is how many refs' rounds the bot keeps; past it the oldest is dropped (a ref with progress kept gets its
// round back from the progress: roundLocked).
const roundsMost = 256

// sendRound is the latest content sent under a ref (its messageSum) and which round of the ref it is.
type sendRound struct {
	sum string
	n   int
}

// roundLocked is the round of ref's send of the content sum, b.mu held: the latest round when sum is the latest
// content sent under ref (the hub sends it again), the next round when it is not. Without a ref it is 0. A ref whose
// round was dropped while its progress is still kept (the hub sends it again after a failure) takes the progress's
// round as the latest, so the same content goes on from where it stopped and other content takes the next round.
func (b *Bot) roundLocked(ref, sum string) int {
	if ref == "" {
		return 0
	}
	r, ok := b.rounds[ref]
	if ok && r.sum == sum {
		return r.n
	}
	if b.rounds == nil {
		b.rounds = map[string]sendRound{}
	}
	if !ok {
		b.roundOrder = append(b.roundOrder, ref)
		for len(b.roundOrder) > roundsMost {
			delete(b.rounds, b.roundOrder[0])
			b.roundOrder = b.roundOrder[1:]
		}
		if kept, has := b.progress[ref]; has {
			r = sendRound{sum: kept.sum, n: kept.round}
		}
	}
	if r.sum != sum {
		r = sendRound{sum: sum, n: r.n + 1}
	}
	b.rounds[ref] = r
	return r.n
}

// sendKey tells one channel.send under a ref from another at the program's entry (sentRefs): the sending generation
// the call came in (Bot.sendGen) and the message's content, so a send to a WeChat bound later is not answered with
// what was sent to one before.
func sendKey(gen uint64, p wire.ChannelSendParams) string {
	return strconv.FormatUint(gen, 10) + ":" + messageSum(splitText(p.Text), p.Images)
}

// messageSum is a digest of a send's text pieces and pictures: what tells one message under a ref from another.
func messageSum(parts []string, images []wire.ChannelBlob) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "text %d\n", len(part))
		_, _ = io.WriteString(h, part)
	}
	for _, img := range images {
		_, _ = fmt.Fprintf(h, "image %d\n", len(img.Data))
		_, _ = io.WriteString(h, img.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// note is the client_id of the text that says n pictures were not sent.
func (s sendIDs) note(n int) string { return s.id("note", n) }

// id is the client_id of the piece of kind at place i; random without a ref.
func (s sendIDs) id(kind string, i int) string {
	if s.ref == "" {
		var r [12]byte
		_, _ = rand.Read(r[:])
		return "persona-" + hex.EncodeToString(r[:])
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%s|%d:%s|%d|%s|%s|%d", len(s.ref), s.ref, len(s.peer), s.peer, s.round, s.sum, kind, i))
	return "persona-" + hex.EncodeToString(sum[:12])
}

// progressMost is how many refs' progress the bot keeps; past it the oldest is dropped (its send then starts over).
const progressMost = 64

// sendProgress is how far a ref's channel.send of the content sum in round got, by client_id (sendIDs).
type sendProgress struct {
	// sum and round are the send's (sendIDs).
	sum   string
	round int
	// done are the text pieces and pictures WeChat confirmed, and the pictures skipped: none is sent again.
	done map[string]bool
	// skipped are the pictures skipped as unsendable, for the text that says how many were not sent.
	skipped map[string]bool
	// upload is picture uploadOf on WeChat's CDN, which a try uploaded but did not send: the next try only sends it.
	upload   ilink.Uploaded
	uploadOf string
}

// uploaded is picture id's upload kept from an earlier try; the zero Uploaded when there is none.
func (pr sendProgress) uploaded(id string) ilink.Uploaded {
	if pr.uploadOf == id {
		return pr.upload
	}
	return ilink.Uploaded{}
}

// countIn is how many of ids are in set.
func countIn(set map[string]bool, ids []string) int {
	n := 0
	for _, id := range ids {
		if set[id] {
			n++
		}
	}
	return n
}

// resume is what is kept of ref's send for a send that started in sending generation gen (Bot.sendGen). What was kept
// for other content under the ref, or another round of it, is dropped: that send starts over. Nothing is kept without a ref, or
// for a send that started before the binding changed.
func (b *Bot) resume(ref string, gen uint64, ids sendIDs) sendProgress {
	pr := sendProgress{sum: ids.sum, round: ids.round, done: map[string]bool{}, skipped: map[string]bool{}}
	if ref == "" {
		return pr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if gen != b.sendGen {
		return pr
	}
	kept, ok := b.progress[ref]
	switch {
	case !ok:
		return pr
	case kept.sum != ids.sum || kept.round != ids.round:
		b.forgetLocked(ref)
		return pr
	}
	maps.Copy(pr.done, kept.done)
	maps.Copy(pr.skipped, kept.skipped)
	pr.upload, pr.uploadOf = kept.upload, kept.uploadOf
	return pr
}

// keep records pr as how far ref's send got: it failed, and the hub sends the ref again. A send that started in another
// sending generation (gen) than the bot's now went to a binding no longer there: what it got is not kept.
func (b *Bot) keep(ref string, gen uint64, pr sendProgress) {
	if ref == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if gen != b.sendGen {
		return
	}
	if b.progress == nil {
		b.progress = map[string]sendProgress{}
	}
	if _, ok := b.progress[ref]; !ok {
		b.progressOrder = append(b.progressOrder, ref)
		for len(b.progressOrder) > progressMost {
			delete(b.progress, b.progressOrder[0])
			b.progressOrder = b.progressOrder[1:]
		}
	}
	b.progress[ref] = pr
}

// forget drops ref's progress: its send is done (sentRefs remembers that from here on).
func (b *Bot) forget(ref string) {
	if ref == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forgetLocked(ref)
}

func (b *Bot) forgetLocked(ref string) {
	delete(b.progress, ref)
	b.progressOrder = slices.DeleteFunc(b.progressOrder, func(r string) bool { return r == ref })
}

// forgetSendsLocked drops every ref's progress, b.mu held: what it says was sent went to a WeChat no longer bound. It
// starts a new sending generation, so a send still running for that WeChat does not keep its progress when it ends.
func (b *Bot) forgetSendsLocked() {
	b.progress, b.progressOrder = nil, nil
	b.sendGen++
}

// imageResult is how sending one picture ended.
type imageResult int

const (
	imageSent imageResult = iota
	// imageSkipped: the picture cannot be sent (not a picture, too large, or refused by WeChat in a way that does not
	// pass); the pictures after it go on.
	imageSkipped
	// imageFailed: a failure that may pass remains after the picture's tries; the send stops at it, and the hub's next
	// send of the ref goes on from it.
	imageFailed
)

// sendImage uploads one of the hub's pictures (the i-th of n) to WeChat's CDN and sends it with id as its client_id,
// trying a failure that may pass again (retry). *upload is the picture once the CDN took it (zero before): a try after
// that only sends it, and Send keeps it for the hub's next send of the ref. Its final result is logged like a text
// piece's, with image=i/n in place of part=i/n (never the picture, its upload URL or keys); a picture WeChat confirmed
// goes into the quote index.
func (b *Bot) sendImage(ctx context.Context, c *ilink.Client, peer, contextToken, replyTo, ref, id string, i, n int, img wire.ChannelBlob, upload *ilink.Uploaded) imageResult {
	started := time.Now()
	head := fmt.Sprintf("wechat: send reply_to=%q ref=%q image=%d/%d", replyTo, ref, i+1, n)
	data, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil || len(data) == 0 || len(data) > ilink.MaxMedia {
		reason := "not_base64"
		switch {
		case err == nil && len(data) == 0:
			reason = "empty"
		case err == nil:
			reason = "too_large"
		}
		log.Printf("%s result=skipped reason=%s duration=%s", head, reason, time.Since(started))
		return imageSkipped
	}
	var serverID string
	err = retry(ctx, func() (err error) {
		if upload.Query == "" {
			if *upload, err = c.UploadImage(ctx, peer, data); err != nil {
				return err
			}
		}
		serverID, err = c.SendImage(ctx, peer, contextToken, id, *upload)
		return err
	})
	took := time.Since(started)
	switch {
	case err == nil:
		log.Printf("%s result=success duration=%s", head, took)
		b.rememberOutbound(peer, serverID, "", []quoteMedia{{Kind: string(ilink.MediaImage)}})
		return imageSent
	case !ilink.Retryable(err):
		log.Printf("%s result=skipped reason=refused duration=%s error_type=%s error=%v", head, took, ilink.ErrorType(err), err)
		return imageSkipped
	}
	log.Printf("%s result=failure duration=%s error_type=%s error=%v", head, took, ilink.ErrorType(err), err)
	return imageFailed
}

// sendFailed: WeChat did not take the message (logged by the hub, which sends it again with the same ref).
type sendFailed struct{ lang string }

func (e sendFailed) Error() string { return t(e.lang, "chat.send_failed") }

// empty says whether m has nothing to hand on.
func empty(m wire.ChannelMessage) bool {
	return strings.TrimSpace(m.Text) == "" && len(m.Images) == 0 && len(m.Files) == 0 && m.Quote == "" && m.Forward == "" &&
		len(m.Unsupported) == 0
}

// compose is the owner's message as the hub takes it: the text (including a voice message's transcript when WeChat
// provides one), the quote, the pictures, and downloaded documents, videos and untranscribed voice audio as files.
func (b *Bot) compose(ctx context.Context, c *ilink.Client, account string, m ilink.Message) wire.ChannelMessage {
	out := wire.ChannelMessage{ID: m.ID, Text: strings.TrimSpace(m.Text), Quote: m.Quote}
	if b.quotes != nil {
		media := make([]quoteMedia, 0, len(m.Media))
		for _, md := range m.Media {
			media = append(media, quoteMedia{Kind: string(md.Kind), Name: sanitizeFileName(md.FileName), Size: md.Size})
		}
		b.quotes.put(quoteCacheID(account, m.SvrID), out.Text, media)
		restored := b.quotes.lookup(quoteCacheID(account, m.QuoteID))
		if restored != "" && m.Partial != nil {
			if selected, ok := ilink.ResolvePartialQuote(restored, *m.Partial); ok {
				restored = selected
			} else {
				restored = ""
			}
		}
		if restored != "" {
			out.Quote = joinQuote(out.Quote, restored)
		}
	}
	total := 0
	voiceFiles := 0
	for _, md := range m.Media {
		switch md.Kind {
		case ilink.MediaImage:
			if len(out.Images) >= imagesMost {
				out.Unsupported = append(out.Unsupported, "更多的图片") // i18n-ignore: what the assistant reads
				continue
			}
			data, err := c.Download(ctx, md)
			if err != nil {
				log.Printf("wechat: downloading a picture: %v", err)
				out.Unsupported = append(out.Unsupported, "一张没下载下来的图片") // i18n-ignore: what the assistant reads
				continue
			}
			if len(data) > messageMediaMost-total {
				out.Unsupported = append(out.Unsupported, "一张太大的图片") // i18n-ignore: what the assistant reads
				continue
			}
			out.Images = append(out.Images, wire.ChannelBlob{Mime: http.DetectContentType(data), Data: base64.StdEncoding.EncodeToString(data)})
			total += len(data)
		case ilink.MediaVoice:
			if md.Transcribed {
				continue
			}
			blob, n, unsupported := downloadVoiceFallback(ctx, c, md, messageMediaMost-total, voiceFiles)
			if unsupported != "" {
				out.Unsupported = append(out.Unsupported, unsupported)
				continue
			}
			out.Files = append(out.Files, blob)
			total += n
			voiceFiles++
		case ilink.MediaFile, ilink.MediaVideo:
			name, unsupported := sanitizeFileName(md.FileName), "一段视频" // i18n-ignore: what the assistant reads
			if md.Kind == ilink.MediaVideo {
				name = "video.mp4"
			} else if name == "" {
				name, unsupported = "file", "一个文件" // i18n-ignore: what the assistant reads
			} else {
				unsupported = "文件「" + name + "」" // i18n-ignore: what the assistant reads
			}
			if md.Size > int64(messageMediaMost-total) {
				out.Unsupported = append(out.Unsupported, unsupported)
				continue
			}
			data, err := c.Download(ctx, md)
			if err != nil {
				log.Printf("wechat: downloading a file: %v", err)
				out.Unsupported = append(out.Unsupported, unsupported)
				continue
			}
			if len(data) > messageMediaMost-total {
				out.Unsupported = append(out.Unsupported, unsupported)
				continue
			}
			out.Files = append(out.Files, wire.ChannelBlob{Name: name, Mime: fileMIME(md.Kind, name, data), Data: base64.StdEncoding.EncodeToString(data)})
			total += len(data)
		}
	}
	out.Unsupported = slices.Compact(out.Unsupported)
	return out
}

func sanitizeFileName(raw string) string {
	s := strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")
	if s == "" {
		return ""
	}
	s = path.Base(s)
	if s == "." || s == ".." || s == "/" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			continue
		}
		b.WriteRune(r)
	}
	s = strings.TrimSpace(b.String())
	if s == "." || s == ".." {
		return ""
	}
	return s
}

func quoteCacheID(account, serverID string) string {
	if strings.TrimSpace(serverID) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(account))
	return hex.EncodeToString(digest[:]) + "\x00" + serverID
}

func joinQuote(inline, restored string) string {
	switch {
	case restored == "":
		return inline
	case inline == "":
		return restored
	case inline == restored:
		return inline
	default:
		return inline + "\n" + restored
	}
}

// fileMIME keeps the filename's document type; unknown extensions use the decrypted bytes. iLink videos are MP4.
func fileMIME(kind ilink.MediaKind, fileName string, data []byte) string {
	if kind == ilink.MediaVideo {
		return "video/mp4"
	}
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".pdf":
		return "application/pdf"
	case ".doc":
		return "application/msword"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	default:
		return http.DetectContentType(data)
	}
}

// typing is the owner's chat showing that the bot is writing.
type typing struct {
	ticket string
	at     time.Time // when the hub last asked for it
	shown  time.Time // when WeChat was last told
}

// Typing is the hub's channel.typing: on while a turn goes on (asked again every few seconds), off once answered.
func (b *Bot) Typing(ctx context.Context, on bool) error {
	b.mu.Lock()
	c, creds, contextToken, t := b.client, b.st.Creds, b.st.Context, b.typing
	now := b.opt.Now()
	if !on {
		b.typing = nil
		var ticket string
		if t != nil {
			ticket = t.ticket
		}
		b.mu.Unlock()
		if c == nil || creds == nil || contextToken == "" || ticket == "" {
			return nil // presence is best effort
		}
		return c.SetTyping(ctx, creds.UserID, ticket, false)
	}
	if c == nil || creds == nil || contextToken == "" {
		b.mu.Unlock()
		return nil // presence is best effort
	}
	if t != nil && now.Sub(t.at) < typingFor {
		t.at = now
		if now.Sub(t.shown) < typingEvery {
			b.mu.Unlock()
			return nil
		}
		t.shown = now
		ticket := t.ticket
		b.mu.Unlock()
		return c.SetTyping(ctx, creds.UserID, ticket, true) // WeChat clears it on its own
	}
	b.mu.Unlock()
	ticket, err := c.TypingTicket(ctx, creds.UserID, contextToken)
	if err != nil {
		return nil
	}
	if err := c.SetTyping(ctx, creds.UserID, ticket, true); err != nil {
		return nil
	}
	b.mu.Lock()
	if b.client == c && b.st.Creds != nil && b.st.Creds.Token == creds.Token {
		b.typing = &typing{ticket: ticket, at: now, shown: now}
	}
	b.mu.Unlock()
	return nil
}
