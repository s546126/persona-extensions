package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"
)

//go:embed prompts/cards.md
var cardsPrompt string

const maxMaterial = 12000 // characters of material per generation call

var cardsSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["cards"],"properties":{"cards":{"type":"array","maxItems":20,"items":{"type":"object","additionalProperties":false,"required":["front","back"],"properties":{"front":{"type":"string","maxLength":600},"back":{"type":"string","maxLength":2000}}}}}}`)

type modelAPI interface {
	ModelComplete(ctx context.Context, p wire.ModelCompleteParams) (wire.ModelCompleteResult, error)
}

// makeCards asks the hub's model for up to n cards from material. The material is clipped and fenced; the model's
// answer is checked against the schema here too, since a model may ignore it.
func makeCards(ctx context.Context, m modelAPI, title, material string, n int) ([]cardText, error) {
	if m == nil {
		return nil, errors.New("没有模型可用：在扩展权限里允许使用模型")
	}
	material = strings.TrimSpace(material)
	if material == "" {
		return nil, errors.New("这份材料是空的")
	}
	n = min(max(n, 1), 20)
	material = clip(material, maxMaterial)
	material = strings.ReplaceAll(material, "</material>", "</ material>")
	user := "<material>\n"
	if title != "" {
		user += "# " + strings.ReplaceAll(title, "\n", " ") + "\n\n"
	}
	user += material + "\n</material>"
	res, err := m.ModelComplete(ctx, wire.ModelCompleteParams{
		System:     strings.ReplaceAll(cardsPrompt, "{{N}}", strconv.Itoa(n)),
		Messages:   []wire.ModelMessage{{Role: "user", Content: user}},
		MaxTokens:  4000,
		JSONSchema: cardsSchema,
		Label:      "study.cards",
	})
	if err != nil {
		return nil, err
	}
	raw := res.JSON
	if len(raw) == 0 {
		raw = json.RawMessage(extractJSON(res.Text))
	}
	var out struct {
		Cards []cardText `json:"cards"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("模型没给出能用的卡片")
	}
	cards := out.Cards[:0]
	for _, c := range out.Cards {
		c.Front, c.Back = strings.TrimSpace(c.Front), strings.TrimSpace(c.Back)
		if c.Front != "" && c.Back != "" {
			cards = append(cards, c)
		}
	}
	if len(cards) > n {
		cards = cards[:n]
	}
	return cards, nil
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```")
	s = strings.TrimSuffix(s, "```")
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

// topicMaterial turns a knowledge-base topic into material: its summary, then its knowledge points.
func topicMaterial(d wire.KBDetail) string {
	var b strings.Builder
	if s := strings.TrimSpace(d.Summary); s != "" {
		b.WriteString(s + "\n\n")
	}
	for _, p := range d.Points {
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		b.WriteString("- ")
		if len(p.Path) > 0 {
			b.WriteString(strings.Join(p.Path, " › ") + ": ")
		}
		b.WriteString(strings.TrimSpace(p.Text))
		if w := strings.TrimSpace(p.Why); w != "" {
			b.WriteString(" (" + w + ")")
		}
		b.WriteString("\n")
		if b.Len() > maxMaterial {
			break
		}
	}
	return b.String()
}
