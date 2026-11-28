package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

type config struct {
	Machines     []string `json:"machines"`
	Vault        string   `json:"obsidian_vault"`
	Logseq       string   `json:"logseq_graph"`
	FlashTag     string   `json:"flash_tag"`
	NLMCommand   string   `json:"notebooklm_command"`
	NewPerDay    int      `json:"new_per_day"`
	Retention    float64  `json:"retention"`
	Remind       bool     `json:"remind"`
	RemindAt     string   `json:"remind_at"`
	CardsPerNote int      `json:"cards_per_source"`
	// The learner profile: how to explain, never what to teach (profile.go).
	LearnerLevel     string `json:"learner_level"`
	LearnerInterests string `json:"learner_interests"`
	ExplainStyle     string `json:"explain_style"`
}

func defaults() config {
	return config{FlashTag: "flashcards", NewPerDay: 20, Retention: 0.9, Remind: true, RemindAt: "20:00", CardsPerNote: 8}
}

var clock = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func (c *config) normalize() {
	if c.NewPerDay < 0 || c.NewPerDay > 200 {
		c.NewPerDay = 20
	}
	if c.Retention < 0.7 || c.Retention > 0.97 {
		c.Retention = 0.9
	}
	if !clock.MatchString(c.RemindAt) {
		c.RemindAt = "20:00"
	}
	if c.CardsPerNote < 1 || c.CardsPerNote > 20 {
		c.CardsPerNote = 8
	}
	if strings.TrimSpace(c.FlashTag) == "" {
		c.FlashTag = "flashcards"
	}
	if c.LearnerLevel = strings.ToLower(strings.TrimSpace(c.LearnerLevel)); learnerLevels[c.LearnerLevel] == "" {
		c.LearnerLevel = ""
	}
	if c.ExplainStyle = strings.ToLower(strings.TrimSpace(c.ExplainStyle)); explainStyles[c.ExplainStyle] == "" {
		c.ExplainStyle = ""
	}
	c.LearnerInterests = cleanInterests(c.LearnerInterests)
}

// hubAPI is what the program uses of the hub; *ext.Hub has all of it, and the tests give a fake.
type hubAPI interface {
	modelAPI
	KBList(ctx context.Context, p wire.KBListParams) (wire.KBListResult, error)
	KBGet(ctx context.Context, p wire.KBGetParams) (wire.KBGetResult, error)
	Call(ctx context.Context, method string, params, out any) error
	Invalidate(contribution string) error
	Badge(contribution, text, tone string) error
}

type app struct {
	done   map[string]map[string]any // actions.do results by action name and request id
	busy   map[string]chan struct{}  // actions.do requests running now, by the same key
	mu     sync.Mutex
	cfg    config
	st     state
	path   string
	sched  scheduler
	hub    hubAPI
	notion *notionClient
	notify func(ctx context.Context, n notice) error
	now    func() time.Time
	loc    *time.Location
}

func openApp(dataDir string, cfg config, notionSecret string, hub hubAPI, tz string) (*app, error) {
	cfg.normalize()
	path := ""
	if dataDir != "" {
		path = filepath.Join(dataDir, "study.json")
	}
	st, err := loadState(path)
	if err != nil {
		return nil, err
	}
	loc := time.Local
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return &app{done: map[string]map[string]any{}, busy: map[string]chan struct{}{}, cfg: cfg, st: st, path: path, sched: newScheduler(cfg.Retention), hub: hub,
		notion: newNotion(notionAPI, notionSecret), now: time.Now, loc: loc}, nil
}

// update commits a changed copy of the state, so a failed write never leaves a half-made change in memory.
// Callers hold a.mu.
func (a *app) update(fn func(*state) error) error {
	s := a.st.clone()
	if err := fn(&s); err != nil {
		return err
	}
	if err := saveState(a.path, s); err != nil {
		return fmt.Errorf("没能保存复习记录：%w", err)
	}
	a.st = s
	a.changed()
	return nil
}

// changed refreshes the widget and the page's badge. The hub writes are not made under the lock.
func (a *app) changed() {
	if a.hub == nil {
		return
	}
	due := len(queue(a.st, a.now(), a.loc, a.cfg.NewPerDay))
	go func() {
		badge, tone := "", ""
		if due > 0 {
			badge, tone = fmt.Sprint(due), wire.ToneCaution
		}
		if err := a.hub.Badge(pageID, badge, tone); err != nil {
			log.Printf("学习：角标没更新：%v", err)
		}
		if err := a.hub.Invalidate(widgetID); err != nil {
			log.Printf("学习：小组件没刷新：%v", err)
		}
	}()
}

func (a *app) machine() (string, error) {
	if len(a.cfg.Machines) == 0 || strings.TrimSpace(a.cfg.Machines[0]) == "" {
		return "", errors.New("先在扩展设置里选一台电脑：Obsidian、Logseq 和 NotebookLM 都从那台电脑上读")
	}
	return a.cfg.Machines[0], nil
}

func (a *app) node(ctx context.Context, method string, params, out any) error {
	m, err := a.machine()
	if err != nil {
		return err
	}
	if a.hub == nil {
		return errors.New("还没连上 Persona")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return a.hub.Call(ctx, "nodes.call", map[string]any{"machine": m, "method": "study." + method, "params": params}, out)
}

// ---- adding cards ----

// addDirect adds cards the user wrote themselves (Obsidian flashcard syntax, a card typed on the page).
func (a *app) addDirect(items []cardText, src Source) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	err := a.update(func(s *state) error {
		now := a.now()
		for _, it := range items {
			if _, ok := s.addCard(it.Front, it.Back, it.Topic, src, now); ok {
				n++
			}
		}
		return nil
	})
	return n, err
}

// addDrafts files cards a model or another program made; they wait for the user to keep them.
func (a *app) addDrafts(items []cardText, src Source) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	err := a.update(func(s *state) error {
		n = s.addDrafts(items, src, a.now())
		return nil
	})
	return n, err
}

// generate has the model draft cards from material; topic, when given, files them under that knowledge point.
func (a *app) generate(ctx context.Context, src Source, material, topic string) (int, error) {
	a.mu.Lock()
	n := a.cfg.CardsPerNote
	a.mu.Unlock()
	cards, err := makeCards(ctx, a.model(), src.Title, material, n)
	if err != nil {
		return 0, err
	}
	if len(cards) == 0 {
		return 0, errors.New("这份材料里没找到值得做卡的内容")
	}
	for i := range cards {
		cards[i].Topic = topic
	}
	return a.addDrafts(cards, src)
}

func (a *app) fromTopic(ctx context.Context, id string) (int, error) {
	if a.hub == nil {
		return 0, errors.New("还没连上 Persona")
	}
	if id == "" || len(id) > 200 {
		return 0, errors.New("知识点 ID 不对")
	}
	res, err := a.hub.KBGet(ctx, wire.KBGetParams{Kind: "topics", ID: id})
	if err != nil {
		return 0, fmt.Errorf("没读到这个主题：%w", err)
	}
	name := id
	if res.Detail.ID != "" {
		name = res.Detail.ID
	}
	title := topicName(res.Detail, name)
	return a.generate(ctx, Source{Kind: srcTopic, Ref: id, Title: title}, topicMaterial(res.Detail), title)
}

func topicName(d wire.KBDetail, fallback string) string {
	var m struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	}
	if len(d.Meta) > 0 && jsonUnmarshal(d.Meta, &m) == nil {
		if m.Name != "" {
			return m.Name
		}
		if m.Title != "" {
			return m.Title
		}
	}
	return fallback
}

func (a *app) topics(ctx context.Context) ([]wire.KBEntry, error) {
	if a.hub == nil {
		return nil, errors.New("还没连上 Persona")
	}
	res, err := a.hub.KBList(ctx, wire.KBListParams{Kind: "topics", Limit: 50})
	if err != nil {
		return nil, fmt.Errorf("没读到知识库：%w", err)
	}
	return res.Items, nil
}

func (a *app) fromNotion(ctx context.Context, id string) (int, error) {
	doc, err := a.notion.read(ctx, id)
	if err != nil {
		return 0, err
	}
	src := Source{Kind: srcNotion, Ref: id, Title: doc.Title, URL: safeURL(doc.URL, "www.notion.so", "notion.so")}
	if len(doc.Toggles) > 0 {
		return a.addDrafts(doc.Toggles, src)
	}
	return a.generate(ctx, src, doc.Text, "")
}

func (a *app) importVault(ctx context.Context) (int, int, error) {
	var scan vaultScan
	if err := a.node(ctx, "vault_cards", map[string]any{}, &scan); err != nil {
		return 0, 0, err
	}
	added := 0
	for _, n := range scan.Notes {
		k, err := a.addDirect(n.Cards, Source{Kind: srcObsidian, Ref: n.Path, Title: n.Title})
		if err != nil {
			return added, len(scan.Notes), err
		}
		added += k
	}
	return added, len(scan.Notes), nil
}

func (a *app) fromVault(ctx context.Context, path string) (int, error) {
	var note noteText
	if err := a.node(ctx, "vault_read", map[string]any{"path": path}, &note); err != nil {
		return 0, err
	}
	src := Source{Kind: srcObsidian, Ref: note.Path, Title: note.Title}
	if cards := parseObsidian(note.Text); len(cards) > 0 {
		return a.addDirect(cards, src)
	}
	return a.generate(ctx, src, note.Text, "")
}

// importLogseq adds the #card blocks of every Logseq page and journal; each card is filed under its page.
func (a *app) importLogseq(ctx context.Context) (int, int, error) {
	var scan vaultScan
	if err := a.node(ctx, "logseq_cards", map[string]any{}, &scan); err != nil {
		return 0, 0, err
	}
	added := 0
	for _, n := range scan.Notes {
		for i := range n.Cards {
			n.Cards[i].Topic = n.Title
		}
		k, err := a.addDirect(n.Cards, Source{Kind: srcLogseq, Ref: n.Path, Title: n.Title})
		if err != nil {
			return added, len(scan.Notes), err
		}
		added += k
	}
	return added, len(scan.Notes), nil
}

// fromLogseq adds one page's #card blocks, or has the model draft cards from the page when it has none.
func (a *app) fromLogseq(ctx context.Context, path string) (int, error) {
	var note noteText
	if err := a.node(ctx, "logseq_read", map[string]any{"path": path}, &note); err != nil {
		return 0, err
	}
	src := Source{Kind: srcLogseq, Ref: note.Path, Title: note.Title}
	if cards := parseLogseq(note.Text); len(cards) > 0 {
		for i := range cards {
			cards[i].Topic = note.Title
		}
		return a.addDirect(cards, src)
	}
	return a.generate(ctx, src, logseqText(note.Text), note.Title)
}

func (a *app) fromNotebookLM(ctx context.Context, notebook, artifact, kind, notebookTitle string) (int, error) {
	var set nlmSet
	if err := a.node(ctx, "nlm_download", map[string]any{"notebook": notebook, "artifact": artifact, "kind": kind}, &set); err != nil {
		return 0, err
	}
	title := strings.TrimSpace(notebookTitle)
	if set.Title != "" {
		title = strings.TrimSpace(title + " · " + set.Title)
	}
	for i := range set.Cards {
		set.Cards[i].Topic = "" // filed under the notebook, not under what the export says
	}
	return a.addDrafts(set.Cards, Source{Kind: srcNotebookLM, Ref: notebook + "/" + artifact, Title: strings.Trim(title, " ·")})
}

// safeURL keeps a link only when it is https on one of the hosts the source uses.
func safeURL(raw string, hosts ...string) string {
	for _, h := range hosts {
		if strings.HasPrefix(raw, "https://"+h+"/") {
			return raw
		}
	}
	return ""
}

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }
