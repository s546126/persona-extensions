package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const widgetID = "water-today"

// progressRing is the progress style from nwverse/persona#9457. The SDK vendored in this repository does not define it yet.
const progressRing = "ring"

// progressKey includes the calendar date even when both days have no drinks.
// Widget state is guarded by app.mu; event writes happen outside it.
func (a *app) progressKey(now time.Time) string {
	l := a.s.location()
	return fmt.Sprintf("%s/%s/%d", l, day(now, l), total(a.s, now, l))
}

func (a *app) refreshWidget(now time.Time) {
	a.widgetPending = a.progressKey(now)
	a.wakeWidget()
}

// Callers hold app.mu. A single queued wake coalesces changes without waiting on output.
func (a *app) wakeWidget() {
	if a.widgetPending != a.widgetKey {
		select {
		case a.widgetWake <- struct{}{}:
		default:
		}
	}
}

func (a *app) sendWidgets(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.widgetWake:
		}
		if ctx.Err() != nil {
			return
		}
		a.mu.Lock()
		key := a.widgetPending
		clean := key == a.widgetKey
		a.mu.Unlock()
		if clean {
			continue
		}
		// The SDK writer can block on the hub's stdout reader. Never hold the state lock here.
		if a.invalidate != nil {
			if err := a.invalidate(widgetID); err != nil {
				log.Printf("喝水进度刷新失败：%v", err)
				continue // Keep the pending key dirty; a later tick wakes us again.
			}
		}
		a.mu.Lock()
		// A newer change may have arrived during the write; acknowledge only what we read.
		a.widgetKey = key
		a.wakeWidget()
		a.mu.Unlock()
	}
}

func (a *app) renderWidget(_ context.Context, r *ext.UIRequest) (*wire.View, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if err := a.remember(r, now); err != nil {
		return nil, err
	}
	return a.widget(r.Family(), now)
}

func (a *app) widget(family string, now time.Time) (*wire.View, error) {
	c, s := a.cfg, a.s
	l := s.location()
	now = now.In(l)
	n := total(s, now, l)
	detail := fmt.Sprintf("还差约 %d 杯", cups(c, n))
	tone := ""
	if n >= c.Goal {
		detail, tone = "目标达成", "positive"
	}
	progress := wire.Component{Type: "progress", Label: detail,
		Ratio: pluginkit.Ratio(float64(n) / float64(c.Goal)), Detail: detail, Tone: tone}
	var items []wire.Component
	switch family {
	case wire.WidgetFamilyAccessoryCircular:
		progress.Label = "喝水"
		items = []wire.Component{progress}
	case wire.WidgetFamilyAccessoryRectangular:
		// Apple draws only progress.label, with a single line, and ignores detail.
		note := fmt.Sprintf("约%d杯", cups(c, n))
		if n >= c.Goal {
			note = "达标"
		}
		progress.Label = fmt.Sprintf("%d/%d毫升·%s", n, c.Goal, note)
		items = []wire.Component{progress}
	case wire.WidgetFamilyAccessoryInline:
		summary := fmt.Sprintf("喝水 %d/%d 毫升", n, c.Goal)
		if n >= c.Goal {
			summary = "喝水 已达标"
		}
		items = []wire.Component{{Type: "text", Text: summary, Tone: tone}}
	case wire.WidgetFamilySmall, wire.WidgetFamilyMedium:
		progress.Style = progressRing
		items = []wire.Component{{Type: "metric", Label: "今天喝水", Value: fmt.Sprintf("%d / %d 毫升", n, c.Goal), Tone: tone}, progress}
		if family == wire.WidgetFamilyMedium {
			points := []wire.ChartPoint{}
			for i := 6; i >= 0; i-- {
				d := now.AddDate(0, 0, -i)
				points = append(points, wire.ChartPoint{Label: d.Format("01-02"), Value: float64(total(s, d, l))})
			}
			items = append(items, wire.Component{Type: "chart", Kind: "bar", Title: "最近 7 天", Unit: "毫升", Series: []wire.ChartSeries{{Points: points}}})
		}
	default:
		return nil, fmt.Errorf("不支持的小组件尺寸")
	}
	return &wire.View{Sections: []wire.Section{{Items: items}}}, nil
}
