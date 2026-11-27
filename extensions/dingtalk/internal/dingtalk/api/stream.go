package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

// Stream mode: the app asks the open platform for a connection (POST /v1.0/gateway/connections/open with its
// credentials and the topics it wants), dials the WebSocket endpoint it is given with the one-time ticket, and reads
// JSON frames. Every frame is answered with its messageId: a SYSTEM ping with the same data (a pong), a CALLBACK with
// code 200 once taken (DingTalk delivers an unanswered one again). A SYSTEM disconnect says the server is moving
// the connection: the app connects again.

// BotTopic is the topic of messages people send to the robot.
const BotTopic = "/v1.0/im/bot/messages/get"

const (
	frameSystem   = "SYSTEM"
	frameCallback = "CALLBACK"
	topicPing     = "ping"
	topicGone     = "disconnect"
	// readLimit bounds one frame (a message with a long rich text).
	readLimit = 4 << 20
	// pingEvery is how often the app checks the connection itself (a WebSocket ping), pingWait how long the
	// server has to answer.
	pingEvery = 60 * time.Second
	pingWait  = 10 * time.Second
	writeWait = 5 * time.Second
)

// Frame is one frame DingTalk sends.
type Frame struct {
	SpecVersion string            `json:"specVersion"`
	Type        string            `json:"type"`
	Time        int64             `json:"time"`
	Headers     map[string]string `json:"headers"`
	Data        string            `json:"data"`
}

type ack struct {
	Code    int               `json:"code"`
	Headers map[string]string `json:"headers"`
	Message string            `json:"message"`
	Data    string            `json:"data"`
}

// ErrMoved: the server asked the app to connect again (a SYSTEM disconnect).
var ErrMoved = errors.New("DingTalk moved the connection")

// endpoint asks for a connection taking topics.
func (c *Client) endpoint(ctx context.Context, topics []string) (string, error) {
	subs := make([]map[string]string, 0, len(topics))
	for _, t := range topics {
		subs = append(subs, map[string]string{"type": frameCallback, "topic": t})
	}
	var out struct {
		Endpoint string `json:"endpoint"`
		Ticket   string `json:"ticket"`
	}
	if err := c.call(ctx, "/v1.0/gateway/connections/open", false, map[string]any{
		"clientId": c.opt.ClientID, "clientSecret": c.opt.ClientSecret, "subscriptions": subs, "ua": c.opt.Agent,
	}, &out); err != nil {
		return "", err
	}
	u, err := url.Parse(out.Endpoint)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || out.Ticket == "" {
		return "", errors.New("DingTalk gave no usable Stream endpoint")
	}
	if !c.webhookHost(u.Hostname()) {
		return "", fmt.Errorf("DingTalk's Stream endpoint is on an unexpected host %q", u.Hostname())
	}
	q := u.Query()
	q.Set("ticket", out.Ticket)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Stream holds one Stream connection for topics until it fails, the server moves it (ErrMoved) or ctx ends.
// connected is called once the connection is up; take is called for each callback frame, one at a time and in the
// order they came. A frame is answered only when its taker calls done (once, from any goroutine, after the frame was
// handled); one never answered (not taken, failed, or lost with the process) is delivered by DingTalk again.
func (c *Client) Stream(ctx context.Context, topics []string, connected func(), take func(ctx context.Context, f Frame, done func())) error {
	addr, err := c.endpoint(ctx, topics)
	if err != nil {
		return err
	}
	// the dial's context bounds the handshake only: the established connection outlives it (coder/websocket)
	dctx, cancel := context.WithTimeout(ctx, c.opt.DialTimeout)
	conn, resp, err := websocket.Dial(dctx, addr, &websocket.DialOptions{HTTPClient: c.opt.HTTP})
	cancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("connecting to DingTalk's Stream endpoint: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)
	if connected != nil {
		connected()
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	go keepAlive(ctx, conn, stop)
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return fmt.Errorf("reading DingTalk's Stream: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var f Frame
		if json.Unmarshal(data, &f) != nil || f.Headers == nil {
			continue
		}
		id := f.Headers["messageId"]
		switch {
		case f.Type == frameSystem && f.Headers["topic"] == topicPing:
			if err := write(ctx, conn, ack{Code: 200, Headers: map[string]string{"contentType": "application/json", "messageId": id},
				Message: "ok", Data: f.Data}); err != nil {
				return err
			}
		case f.Type == frameSystem && f.Headers["topic"] == topicGone:
			_ = write(ctx, conn, ack{Code: 200, Headers: map[string]string{"contentType": "application/json", "messageId": id}})
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return ErrMoved
		case f.Type == frameCallback:
			if take == nil {
				continue
			}
			take(ctx, f, func() {
				// answered late, from the taker's goroutine: a connection gone meanwhile leaves the frame unanswered,
				// and DingTalk delivers it again (the taker dedups)
				_ = write(context.WithoutCancel(ctx), conn, ack{Code: 200, Headers: map[string]string{"contentType": "application/json", "messageId": id},
					Data: `{"response":null}`})
			})
		default:
			// another type or topic it did not ask for: answered as the SDKs do, so it is not delivered again
			if err := write(ctx, conn, ack{Code: 404, Headers: map[string]string{"contentType": "application/json", "messageId": id}}); err != nil {
				return err
			}
		}
	}
}

func write(ctx context.Context, conn *websocket.Conn, a ack) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeWait)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("answering DingTalk's Stream: %w", err)
	}
	return nil
}

// errNoPong: the server did not answer a ping in pingWait.
var errNoPong = errors.New("DingTalk's Stream stopped answering pings")

// keepAlive pings the server every pingEvery and ends the connection (stop) when it does not answer in pingWait.
func keepAlive(ctx context.Context, conn *websocket.Conn, stop context.CancelCauseFunc) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pingWait)
		err := conn.Ping(pctx)
		cancel()
		if err != nil && ctx.Err() == nil {
			stop(errNoPong)
			return
		}
	}
}
