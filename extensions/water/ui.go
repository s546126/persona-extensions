package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Days   int `json:"days"`
	Offset int `json:"offset,omitempty"`
}

func readPage(s string) pageState {
	p := pageState{Days: 7}
	_ = json.Unmarshal([]byte(s), &p)
	if p.Days != 30 {
		p.Days = 7
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

func (a *app) remember(r *ext.UIRequest, now time.Time) error {
	if r.TZ == "" || r.TZ == a.s.Zone {
		return nil
	}
	if _, err := time.LoadLocation(r.TZ); err != nil {
		return nil
	}
	return a.update(now, func(s *state) { s.Zone = r.TZ })
}

// lowDistraction reads the ln-mode cache. It runs before app.mu is taken, because the hub request can
// take seconds. The clock here only ages that cache; every state decision reads the clock under app.mu.
func (a *app) lowDistraction(ctx context.Context) *bool {
	return a.hub.lowDistraction(ctx, a.now())
}

func (a *app) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	ln := a.lowDistraction(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if err := a.remember(r, now); err != nil {
		return nil, err
	}
	return a.view(readPage(r.State), r.UIVersion, now, ln), nil
}

func text(s string) wire.Component { return wire.Component{Type: "text", Text: s} }
func caution(s string) wire.Component {
	return wire.Component{Type: "metric", Label: "提醒", Value: s, Tone: "caution"}
}
func button(title, id string) wire.Component {
	return wire.Component{Type: "button", Title: title, Action: &wire.Action{ID: id}}
}

// deliveryNote turns a stored delivery reason into the sentence the page shows. Hub codes and
// status numbers never reach the page.
func deliveryNote(reason string) string {
	switch reason {
	case "no_device":
		return "暂时没有能收到通知的设备"
	case "filtered":
		return "设备的通知设置挡住了这条提醒"
	case "muted":
		return "通知被静音了"
	case reasonUnreachable:
		return "暂时连不上提醒服务，稍后会再试"
	case reasonRejected:
		return "这次提醒没有发出去"
	case reasonUnconfirmed:
		return "这次提醒没能确认送出，已停止重试"
	default:
		return "提醒没有送达"
	}
}

func (a *app) status(now time.Time) string {
	p := schedule(a.cfg, a.s, now)
	switch p.Reason {
	case "off":
		return "提醒已关闭，可在扩展设置里打开"
	case "invalid":
		return "开始时间要早于结束时间"
	case "done":
		return "今天不再提醒，已达标"
	case "weekend":
		return "周末不提醒，周一 " + p.Next.Format("15:04") + " 开始"
	case "closed":
		if day(p.Next, a.s.location()) == day(now.In(a.s.location()).AddDate(0, 0, 1), a.s.location()) {
			return "今天的提醒时间过了，明天 " + p.Next.Format("15:04") + " 开始"
		}
		return "今天的提醒时间过了，周一 " + p.Next.Format("15:04") + " 开始"
	}
	if a.s.Pending != nil {
		return "下次提醒 " + a.s.Pending.RetryAt.In(a.s.location()).Format("15:04")
	}
	return "下次提醒 " + p.Next.Format("15:04")
}

// view builds the page and must be called under app.mu. ln is the low-distraction value read before the
// lock was taken; nil means unknown. view never does I/O.
func (a *app) view(p pageState, version int, now time.Time, ln *bool) *wire.View {
	c, s := a.cfg, a.s
	l := s.location()
	now = now.In(l)
	n := total(s, now, l)
	detail := fmt.Sprintf("%d / %d 毫升 · 还差约 %d 杯", n, c.Goal, cups(c, n))
	tone := ""
	if n >= c.Goal {
		detail = "目标达成"
		tone = "positive"
	}
	today := []wire.Component{{Type: "progress", Label: "今天喝了多少", Ratio: pluginkit.Ratio(float64(n) / float64(c.Goal)), Detail: detail, Tone: tone}, text(a.status(now)), text("按 " + l.String() + " 时间提醒和记录")}
	if !a.hub.can() {
		today = append(today, caution("没有通知权限，暂时不能发提醒；仍可记录喝水"))
	}
	if ln != nil && *ln {
		today = append(today, caution("低打扰模式开着：提醒会攒进简报，不会响"))
	}
	if schedule(c, s, now).Reason == "invalid" {
		today = append(today, caution("开始时间要早于结束时间"))
	}
	if s.Reason != "" {
		today = append(today, caution(deliveryNote(s.Reason)))
	}
	add := []wire.Component{button(fmt.Sprintf("喝了一杯（%d 毫升）", c.Cup), "drink"), {Type: "field", Name: "amount", Label: "其他量（毫升）", Kind: "number", Placeholder: "比如 300"}, button("记下", "log")}
	add[2].Action.Submit = true
	rows := []wire.Component{}
	var records []record
	for i := len(s.Records) - 1; i >= 0; i-- {
		if day(s.Records[i].At, l) == day(now, l) {
			records = append(records, s.Records[i])
		}
	}
	// Bound each view even when somebody logs hundreds of small drinks in one day.
	if p.Offset < 0 || p.Offset >= len(records) {
		p.Offset = 0
	}
	for _, r := range records[p.Offset:min(p.Offset+40, len(records))] {
		action := &wire.Action{ID: "undo", Confirm: "删掉这条记录？", Values: map[string]string{"id": r.ID}}
		if version < 4 {
			action.ID = "undo:" + r.ID
			action.Values = nil
		}
		rows = append(rows, wire.Component{Type: "row", Title: r.At.In(l).Format("15:04") + fmt.Sprintf(" · %d 毫升", r.ML), Value: "撤销", Action: action})
	}
	if len(rows) == 0 {
		rows = append(rows, text("今天还没有记录，喝一杯就记下来吧"))
	}
	if p.Offset+40 < len(records) {
		rows = append(rows, button("更早记录", "older"))
	}
	if p.Offset > 0 {
		rows = append(rows, button("最新记录", "latest"))
	}
	points := []wire.ChartPoint{}
	daysDone := 0
	for i := p.Days - 1; i >= 0; i-- {
		d := now.AddDate(0, 0, -i)
		ml := total(s, d, l)
		if ml >= c.Goal {
			daysDone++
		}
		points = append(points, wire.ChartPoint{Label: d.Format("01-02"), Value: float64(ml)})
	}
	recent := []wire.Component{{Type: "segmented", Name: "days", Value: strconv.Itoa(p.Days), Choices: []wire.Choice{{Value: "7", Title: "7 天"}, {Value: "30", Title: "30 天"}}, Action: &wire.Action{ID: "range"}},
		{Type: "chart", Kind: "bar", Title: "每天喝了多少", Unit: "毫升", Series: []wire.ChartSeries{{Name: "喝水量", Points: points}}}, text(fmt.Sprintf("目标 %d 毫升 · 达标 %d 天", c.Goal, daysDone))}
	return &wire.View{Title: "喝水", State: ext.EncodeState(p), RefreshAfter: 30, Sections: []wire.Section{
		{Title: "今天", Items: today}, {Title: "记录", Items: add}, {Title: "今天的记录", Items: rows},
		{Title: "最近", Items: recent, Footer: "目标、提醒时间和间隔在「设置 › 扩展 › 喝水提醒」里改。"},
	}}
}

var amountPattern = regexp.MustCompile(`^\s*([0-9]+)\s*(?i:ml|毫升)?\s*$`)

func amount(s string) (int, bool) {
	m := amountPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil && n >= 1 && n <= 3000
}

func progressToast(c config, s state, now time.Time) string {
	n := total(s, now, s.location())
	return fmt.Sprintf("今天 %d / %d 毫升", n, c.Goal)
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	ln := a.lowDistraction(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if err := a.remember(r, now); err != nil {
		return nil, err
	}
	p := readPage(r.State)
	toast := ""
	action := r.Action
	id := r.Values["id"]
	if strings.HasPrefix(action, "undo:") {
		id = strings.TrimPrefix(action, "undo:")
		action = "undo"
	}
	key := ""
	if strings.HasPrefix(r.State, "r=") {
		stamp, err := strconv.ParseInt(strings.TrimPrefix(r.State, "r="), 10, 64)
		if err != nil || now.Sub(time.UnixMilli(stamp)) >= 24*time.Hour || stamp > now.UnixMilli() {
			toast = "这条提醒已过期，请在页面上记录"
		} else {
			key = r.State + ":" + action
			if t, ok := a.s.Seen[key]; ok && now.Sub(t) < 24*time.Hour {
				toast = progressToast(a.cfg, a.s, now)
			}
		}
	}
	if toast == "" {
		err := a.update(now, func(s *state) {
			applied := false
			switch action {
			case "drink", "log":
				ml := a.cfg.Cup
				ok := true
				if action == "log" {
					v, exists := r.Values["amount"]
					if !exists {
						v = r.Values["text"]
					}
					ml, ok = amount(v)
				}
				if !ok {
					toast = "请输入 1–3000 毫升"
					break
				}
				logDrink(s, now, ml)
				applied = true
				p.Offset = 0
				toast = fmt.Sprintf("已记 %d 毫升，%s", ml, progressToast(a.cfg, *s, now))
				if total(*s, now, s.location()) >= a.cfg.Goal {
					toast = "今天的目标达成了"
					if a.cfg.Stop {
						toast += "，今天不再提醒"
					}
				}
			case "snooze":
				s.Snooze = now.Add(30 * time.Minute)
				s.Pending = nil
				applied = true
				toast = "好的，30 分钟后再提醒你"
				if day(schedule(a.cfg, *s, now).Next, s.location()) != day(now, s.location()) {
					toast = "今天不再提醒，下个提醒日再提醒你"
				}
			case "undo":
				toast = "这条记录已找不到了"
				for i, rec := range s.Records {
					if rec.ID == id && day(rec.At, s.location()) == day(now, s.location()) {
						s.Records = append(s.Records[:i], s.Records[i+1:]...)
						toast = "已撤销这条记录"
						applied = true
						break
					}
				}
			case "range":
				p.Days = 7
				if r.Values["days"] == "30" {
					p.Days = 30
				}
			case "older":
				todayCount := 0
				l := s.location()
				todayDay := day(now, l)
				for _, rec := range s.Records {
					if day(rec.At, l) == todayDay {
						todayCount++
					}
				}
				if p.Offset < 0 || p.Offset >= todayCount || todayCount-p.Offset <= 40 {
					p.Offset = 0
				} else {
					p.Offset += 40
				}
			case "latest":
				p.Offset = 0
			default:
				toast = "请在页面上选择操作"
			}
			if applied && key != "" {
				if s.Seen == nil {
					s.Seen = map[string]time.Time{}
				}
				s.Seen[key] = now
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return &wire.ActionResult{Toast: toast, View: a.view(p, r.UIVersion, now, ln)}, nil
}
