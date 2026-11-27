package wecom

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/wecom/internal/wecom/api/apitest"
	"github.com/nwverse/persona/sdk/go/wire"
)

// send is channel.send through the program, as the hub calls it (the ref cache included).
func (r *rig) send(p *program, ref, text string) error {
	r.t.Helper()
	raw, _ := json.Marshal(wire.ChannelSendParams{Contribution: ID, Ref: ref, Text: text, Kind: wire.ChannelSendKindAnswer})
	_, err := p.channel(context.Background(), "send", raw)
	return err
}

// A send whose acknowledgement is lost may have been delivered: it is sent neither again by the robot nor by the
// hub's retry of the same ref (review of #314: two identical messages with different req_ids).
func TestLostAcknowledgementIsNotSentAgain(t *testing.T) {
	r := newRig(t)
	r.pair()
	p := &program{bot: r.bot}
	n := len(r.sentTo(ownerID))
	r.f.Set(func(x *apitest.Fake) { x.LoseAnswers = 1 })
	err := r.send(p, "single-ref", "one logical message")
	if err == nil || err.Error() != msg("chat.send_uncertain") {
		t.Fatalf("a lost acknowledgement: %v", err)
	}
	if err2 := r.send(p, "single-ref", "one logical message"); err2 == nil || err2.Error() != err.Error() {
		t.Fatalf("the hub's retry of the ref: %v", err2)
	}
	if got := len(r.sentTo(ownerID)) - n; got != 1 {
		t.Fatalf("one ref produced %d accepted sends, want 1", got)
	}
}

// A refusal (an errcode) was not delivered: the hub may send the ref again, and it then arrives once.
func TestRefusedSendMayBeSentAgain(t *testing.T) {
	r := newRig(t)
	r.pair()
	p := &program{bot: r.bot}
	n := len(r.sentTo(ownerID))
	r.f.Set(func(x *apitest.Fake) { x.Refuse = 45009 })
	if err := r.send(p, "ref-r", "hello"); err == nil || err.Error() != msg("chat.send_failed") {
		t.Fatalf("a refused send: %v", err)
	}
	r.f.Set(func(x *apitest.Fake) { x.Refuse = 0 })
	if err := r.send(p, "ref-r", "hello"); err != nil {
		t.Fatal(err)
	}
	if got := len(r.sentTo(ownerID)) - n; got != 1 {
		t.Fatalf("delivered %d times, want 1", got)
	}
}

// A long message whose later piece goes unacknowledged keeps its ref: the hub's retry delivers nothing again.
func TestPartlySentMessageIsNotSentAgain(t *testing.T) {
	r := newRig(t)
	r.pair()
	p := &program{bot: r.bot}
	n := len(r.f.Sent())
	long := strings.Repeat("line of text\n", MaxText/10)  // two pieces
	r.f.Set(func(x *apitest.Fake) { x.LoseFrom = n + 2 }) // the first piece is acknowledged, the second is not
	if err := r.send(p, "ref-p", long); err == nil || err.Error() != msg("chat.send_uncertain") {
		t.Fatalf("an unacknowledged second piece: %v", err)
	}
	r.f.Set(func(x *apitest.Fake) { x.LoseFrom = 0 })
	_ = r.send(p, "ref-p", long)
	if got := len(r.f.Sent()) - n; got != 2 {
		t.Fatalf("pieces delivered %d after the hub's retry, want 2 (no piece twice)", got)
	}
}

// channel.send with no text is not reported as delivered.
func TestEmptySendIsNotDelivered(t *testing.T) {
	r := newRig(t)
	r.pair()
	p := &program{bot: r.bot}
	n := len(r.f.Sent())
	for _, text := range []string{"", "  \n\t "} {
		if err := r.send(p, "ref-e"+text, text); err == nil {
			t.Errorf("%q was taken as sent", text)
		}
	}
	if got := len(r.f.Sent()) - n; got != 0 {
		t.Fatalf("sent %d messages", got)
	}
}
