package api

import (
	"encoding/json"
	"strings"
)

// A message someone sent the robot (BotTopic's data), the fields this extension reads.

// Conversation types: a one-to-one chat with the robot, or a group that @-mentions it.
const (
	ConversationSingle = "1"
	ConversationGroup  = "2"
)

// Message kinds (msgtype).
const (
	KindText     = "text"
	KindRichText = "richText"
	KindPicture  = "picture"
	KindAudio    = "audio"
	KindVideo    = "video"
	KindFile     = "file"
)

// Message is a message to the robot.
type Message struct {
	ID               string `json:"msgId"`
	ConversationType string `json:"conversationType"`
	ConversationID   string `json:"conversationId"`
	// SenderStaffID is the sender's id in the organization (the one the robot sends to); empty for someone outside it.
	SenderStaffID string `json:"senderStaffId"`
	SenderNick    string `json:"senderNick"`
	RobotCode     string `json:"robotCode"`
	// SessionWebhook answers in the chat the message came from until SessionWebhookExpiredTime (Unix ms).
	SessionWebhook            string `json:"sessionWebhook"`
	SessionWebhookExpiredTime int64  `json:"sessionWebhookExpiredTime"`
	Kind                      string `json:"msgtype"`
	Text                      struct {
		Content string `json:"content"`
	} `json:"text"`
	Content json.RawMessage `json:"content"`
}

// Part is one piece of what a message carries: text, or something to download (a picture, a file, a voice clip).
type Part struct {
	Kind         string // KindText, KindPicture, KindAudio, KindVideo, KindFile
	Text         string // a text piece, or a voice clip's transcript
	DownloadCode string
	FileName     string
}

type content struct {
	DownloadCode string `json:"downloadCode"`
	PictureCode  string `json:"pictureDownloadCode"`
	FileName     string `json:"fileName"`
	Recognition  string `json:"recognition"`
	RichText     []struct {
		Text         string `json:"text"`
		Type         string `json:"type"`
		DownloadCode string `json:"downloadCode"`
		PictureCode  string `json:"pictureDownloadCode"`
	} `json:"richText"`
}

// Parts are what m carries, in order. A kind it does not know is one part of that kind and nothing else.
func (m Message) Parts() []Part {
	var c content
	if len(m.Content) > 0 {
		_ = json.Unmarshal(m.Content, &c)
	}
	code := func(dc, pc string) string {
		if dc != "" {
			return dc
		}
		return pc
	}
	switch m.Kind {
	case KindText:
		return []Part{{Kind: KindText, Text: m.Text.Content}}
	case KindRichText:
		var parts []Part
		for _, r := range c.RichText {
			switch {
			case r.Text != "":
				parts = append(parts, Part{Kind: KindText, Text: r.Text})
			case code(r.DownloadCode, r.PictureCode) != "":
				parts = append(parts, Part{Kind: KindPicture, DownloadCode: code(r.DownloadCode, r.PictureCode)})
			}
		}
		return parts
	case KindPicture:
		return []Part{{Kind: KindPicture, DownloadCode: code(c.DownloadCode, c.PictureCode)}}
	case KindAudio:
		return []Part{{Kind: KindAudio, Text: c.Recognition, DownloadCode: c.DownloadCode}}
	case KindVideo:
		return []Part{{Kind: KindVideo, DownloadCode: c.DownloadCode}}
	case KindFile:
		return []Part{{Kind: KindFile, DownloadCode: c.DownloadCode, FileName: c.FileName}}
	}
	return []Part{{Kind: m.Kind}}
}

// PlainText is the text m carries, its pieces joined, trimmed.
func (m Message) PlainText() string {
	var b strings.Builder
	for _, p := range m.Parts() {
		if p.Kind == KindText {
			b.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(b.String())
}
