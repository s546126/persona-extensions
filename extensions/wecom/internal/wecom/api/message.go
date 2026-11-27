package api

import (
	"encoding/json"
	"strings"
)

// A message someone sent the robot (aibot_msg_callback's body), the fields this extension reads.

// Chat types.
const (
	ChatSingle = "single"
	ChatGroup  = "group"
)

// Message kinds (msgtype).
const (
	KindText  = "text"
	KindImage = "image"
	KindMixed = "mixed"
	KindVoice = "voice"
	KindFile  = "file"
	KindVideo = "video"
)

type media struct {
	URL      string `json:"url"`
	AESKey   string `json:"aeskey"`
	FileName string `json:"filename"`
}

type content struct {
	Kind string `json:"msgtype"`
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
	Image media `json:"image"`
	File  media `json:"file"`
	Video media `json:"video"`
	Voice struct {
		Content string `json:"content"`
	} `json:"voice"`
	Mixed struct {
		Items []content `json:"msg_item"`
	} `json:"mixed"`
}

// Message is a message to the robot.
type Message struct {
	ID       string `json:"msgid"`
	BotID    string `json:"aibotid"`
	ChatID   string `json:"chatid"`
	ChatType string `json:"chattype"`
	From     struct {
		UserID string `json:"userid"`
	} `json:"from"`
	content
	Quote *content `json:"quote"`
}

// Part is one piece of what a message carries: text, or media to download.
type Part struct {
	Kind     string // KindText, KindImage, KindVoice (Text is WeCom's transcript), KindFile, KindVideo
	Text     string
	URL      string
	AESKey   string
	FileName string
}

func (c content) parts() []Part {
	switch c.Kind {
	case KindText:
		return []Part{{Kind: KindText, Text: c.Text.Content}}
	case KindImage:
		return []Part{{Kind: KindImage, URL: c.Image.URL, AESKey: c.Image.AESKey}}
	case KindVoice:
		return []Part{{Kind: KindVoice, Text: c.Voice.Content}}
	case KindFile:
		return []Part{{Kind: KindFile, URL: c.File.URL, AESKey: c.File.AESKey, FileName: c.File.FileName}}
	case KindVideo:
		return []Part{{Kind: KindVideo, URL: c.Video.URL, AESKey: c.Video.AESKey}}
	case KindMixed:
		var out []Part
		for _, it := range c.Mixed.Items {
			if it.Kind != KindMixed { // no nesting
				out = append(out, it.parts()...)
			}
		}
		return out
	}
	return []Part{{Kind: c.Kind}}
}

// Parts are what m carries, in order.
func (m Message) Parts() []Part { return m.content.parts() }

// PlainText is the text m carries, its pieces joined, trimmed (a voice message's transcript included).
func (m Message) PlainText() string { return textOf(m.Parts()) }

// QuoteText is the text of the message m quotes, if any ("" for none or one without text).
func (m Message) QuoteText() string {
	if m.Quote == nil {
		return ""
	}
	return textOf(m.Quote.parts())
}

func textOf(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == KindText || p.Kind == KindVoice {
			if b.Len() > 0 && p.Kind == KindVoice {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// ParseMessage reads a message callback's body.
func ParseMessage(body json.RawMessage) (Message, error) {
	var m Message
	err := json.Unmarshal(body, &m)
	return m, err
}
