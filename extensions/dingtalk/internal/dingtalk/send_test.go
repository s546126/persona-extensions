package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// send is channel.send through the program, as the hub calls it (the ref cache included).
func (r *rig) send(p *program, ref, text string) error {
	r.t.Helper()
	raw, _ := json.Marshal(wire.ChannelSendParams{Contribution: ID, Ref: ref, Text: text, Kind: wire.ChannelSendKindAnswer})
	_, err := p.channel(context.Background(), "send", raw)
	return err
}

// A send whose answer is lost may have been delivered: it is not sent again, by the robot or by the hub's retry of
// the same ref (reported in review of #314 for WeCom; DingTalk's answer can be lost the same way).
func TestLostAnswerIsNotSentAgain(t *testing.T) {
	sendRetryWait = time.Millisecond
	defer func() { sendRetryWait = time.Second }()
	r := newRig(t, nil)
	r.pair()
	p := &program{bot: r.bot}
	r.f.LoseAnswers = 1
	err := r.send(p, "ref-1", "one logical message")
	var e *ext.Error
	if err == nil || errors.As(err, &e) && e.Code == wire.ChannelErrorCodeCannotSend {
		t.Fatalf("a lost answer: %v", err)
	}
	if err2 := r.send(p, "ref-1", "one logical message"); err2 == nil || err2.Error() != err.Error() {
		t.Fatalf("the hub's retry of the ref: %v", err2)
	}
	if got := r.f.Sent(); len(got) != 1 {
		t.Fatalf("delivered %d times, want 1", len(got))
	}
}

// DingTalk's own refusals that say nothing was taken (rate limit) are sent again; one ref is delivered once.
func TestRateLimitedSendIsRetried(t *testing.T) {
	sendRetryWait = time.Millisecond
	defer func() { sendRetryWait = time.Second }()
	r := newRig(t, nil)
	r.pair()
	p := &program{bot: r.bot}
	r.f.Limit = 2
	if err := r.send(p, "ref-2", "after a rate limit"); err != nil {
		t.Fatal(err)
	}
	if got := r.f.Sent(); len(got) != 1 {
		t.Fatalf("delivered %d times, want 1", len(got))
	}
}

// A long message whose later piece fails keeps its ref: the hub's retry must not deliver the first piece again.
func TestPartlySentMessageIsNotSentAgain(t *testing.T) {
	sendRetryWait = time.Millisecond
	defer func() { sendRetryWait = time.Second }()
	r := newRig(t, nil)
	r.pair()
	p := &program{bot: r.bot}
	long := strings.Repeat("line of text\n", MaxText/10)
	r.f.RefuseAfter = 1 // the second piece is refused outright
	if err := r.send(p, "ref-3", long); err == nil {
		t.Fatal("a refused second piece was taken as sent")
	}
	n := len(r.f.Sent())
	r.f.RefuseAfter = 0
	_ = r.send(p, "ref-3", long)
	if got := len(r.f.Sent()); got != n || n != 1 {
		t.Fatalf("pieces delivered: %d after the retry, %d before (want 1)", got, n)
	}
}

// DingTalk answering a server error with its own code did not take the message: the ref stays retryable.
func TestServerErrorWithACodeIsRetryable(t *testing.T) {
	sendRetryWait = time.Millisecond
	defer func() { sendRetryWait = time.Second }()
	r := newRig(t, nil)
	r.pair()
	p := &program{bot: r.bot}
	r.f.Fail = 503
	if err := r.send(p, "ref-5", "hello"); err == nil || err.Error() != msg("chat.send_failed") {
		t.Fatalf("a 503: %v", err)
	}
	r.f.Fail = 0
	if err := r.send(p, "ref-5", "hello"); err != nil {
		t.Fatal(err)
	}
	if got := len(r.f.Sent()); got != 1 {
		t.Fatalf("delivered %d times, want 1", got)
	}
}

// channel.send with no text is not reported as delivered (review of #311).
func TestEmptySendIsNotDelivered(t *testing.T) {
	r := newRig(t, nil)
	r.pair()
	p := &program{bot: r.bot}
	for _, text := range []string{"", "  \n\t "} {
		if err := r.send(p, "ref-e"+text, text); err == nil {
			t.Errorf("%q was taken as sent", text)
		}
	}
	if n := len(r.f.Sent()); n != 0 {
		t.Fatalf("sent %d messages", n)
	}
}
