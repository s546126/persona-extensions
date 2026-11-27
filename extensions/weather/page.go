package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The page (ui contributions "weather", a nav_page, and "page", a plugin_page): the current weather, the next 24
// hours as a line, the 7-day forecast, and one air quality card. The state it carries is which city is chosen.

type pageState struct {
	City string `json:"c,omitempty"` // the chosen city's name; empty is the first (the default)
}

// readState reads the page's state, keeping the chosen city among the configured ones.
func (a *app) readState(r *ext.UIRequest) pageState {
	_, cities, _ := a.snapshotConfig()
	return readStateFor(r, cities)
}

func readStateFor(r *ext.UIRequest, cities []City) pageState {
	var st pageState
	if r.DecodeState(&st) != nil {
		st = pageState{}
	}
	st.City = strings.TrimSpace(st.City)
	if st.City != "" {
		if c, ok := cityByName(cities, st.City); ok {
			st.City = c.Name
		} else {
			st.City = ""
		}
	}
	return st
}

// selected is the city to draw.
func (a *app) selected(st pageState) City {
	_, cities, _ := a.snapshotConfig()
	c, _ := cityByName(cities, st.City)
	return c
}

func (a *app) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	ctx, cancel := context.WithTimeout(ctx, renderBudget)
	defer cancel()
	cfg, cities, badCities := a.snapshotConfig()
	return a.pageWith(ctx, r, cfg, cities, badCities, readStateFor(r, cities), false), nil
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	ctx, cancel := context.WithTimeout(ctx, renderBudget)
	defer cancel()
	cfg, cities, badCities := a.snapshotConfig()
	st := readStateFor(r, cities)
	refresh := false
	switch {
	case r.Action == "city":
		if name := strings.TrimSpace(r.Values["city"]); name != "" {
			if c, ok := cityByName(cities, name); ok {
				st.City = c.Name
			}
		}
	case strings.HasPrefix(r.Action, "city:"):
		if name := strings.TrimSpace(r.Values["city"]); name != "" {
			if c, ok := cityByName(cities, name); ok {
				st.City = c.Name
			}
		} else if idx, err := strconv.Atoi(strings.TrimPrefix(r.Action, "city:")); err == nil && idx >= 0 && idx < len(cities) {
			st.City = cities[idx].Name
		}
	case r.Action == "refresh":
		refresh = true
	}
	return &wire.ActionResult{View: a.pageWith(ctx, r, cfg, cities, badCities, st, refresh)}, nil
}

func text(s, style string) wire.Component {
	return wire.Component{Type: wire.ComponentText, Text: s, Style: style}
}

func button(title, id string) wire.Component {
	return wire.Component{Type: wire.ComponentButton, Title: title, Action: &wire.Action{ID: id}}
}

const (
	segmentedMaxChoices = 5
	segmentedMaxRunes   = 12
)

// segmentedFits reports whether cities fit within wire.ComponentSegmented limits (2 to 5 choices, each label at most 12 runes).
func segmentedFits(cities []City) bool {
	if len(cities) < 2 || len(cities) > segmentedMaxChoices {
		return false
	}
	for _, c := range cities {
		if utf8.RuneCountInString(c.Name) > segmentedMaxRunes {
			return false
		}
	}
	return true
}

// picker chooses the city when there are several: a segmented control when UI version >= 2 and every city fits
// segmented limits, or a row each otherwise.
func pickerFor(r *ext.UIRequest, cities []City, selected City) wire.Section {
	if len(cities) <= 1 {
		return wire.Section{}
	}
	if r.UIVersion >= 2 && segmentedFits(cities) {
		seg := wire.Component{Type: wire.ComponentSegmented, Name: "city", Value: selected.Name, Action: &wire.Action{ID: "city"}}
		for _, c := range cities {
			seg.Choices = append(seg.Choices, wire.Choice{Value: c.Name, Title: c.Name})
		}
		return wire.Section{Items: []wire.Component{seg}}
	}
	sec := wire.Section{Title: "城市"}
	for i, c := range cities {
		act := &wire.Action{ID: fmt.Sprintf("city:%d", i)}
		if utf8.RuneCountInString(c.Name) <= 1800 {
			act.Values = map[string]string{"city": c.Name}
		}
		it := wire.Component{
			Type:   wire.ComponentRow,
			Title:  clip(c.Name, 1800),
			Action: act,
		}
		if c.Name == selected.Name {
			it.Badge = &wire.Badge{Text: "正在看", Tone: wire.TonePositive}
		}
		sec.Items = append(sec.Items, it)
	}
	return sec
}

func encodeBoundedState(st pageState) string {
	raw := ext.EncodeState(st)
	if len(raw) <= wire.StateMost {
		return raw
	}
	return ext.EncodeState(pageState{})
}

// page is the weather for one city; what went wrong is drawn, with a way to try again, never as a page of zeros.
func (a *app) pageWith(ctx context.Context, r *ext.UIRequest, cfg Config, cities, badCities []City, st pageState, refresh bool) *wire.View {
	c, _ := cityByName(cities, st.City)
	v := &wire.View{Title: "天气", State: encodeBoundedState(st), RefreshAfter: 600}
	if p := pickerFor(r, cities, c); len(p.Items) > 0 {
		v.Sections = append(v.Sections, p)
	}
	if len(badCities) > 0 {
		v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{
			text(clip("这些城市没填对，没有读："+badCityNames(badCities)+"。每项写一个城市名；重名的在后面写上「纬度,经度」，纬度 -90 到 90、经度 -180 到 180", 1900), wire.TextStyleSecondary)}})
	}

	u, w := cfg.tempUnit(), cfg.windUnit()
	located, err := a.locate(ctx, c)
	if err != nil {
		v.Sections = append(v.Sections, wire.Section{Title: clip(c.Name, 1800), Items: []wire.Component{
			text(clip(oneLine(err.Error())+"。换个写法，或在名字后写上「纬度,经度」", 1900), wire.TextStyleSecondary), button("重试", "refresh")}})
		return v
	}
	c = located
	loc := locFor(c)
	f, rd, err := a.readForecast(ctx, c, freshness(refresh))
	if err != nil {
		v.Sections = append(v.Sections, wire.Section{Title: clip(c.Name, 1800), Items: []wire.Component{
			text(clip(oneLine(err.Error()), 1900), wire.TextStyleSecondary), button("重试", "refresh")}})
		return v
	}

	v.Sections = append(v.Sections, a.nowSection(c, f, u, w))

	hours := f.next24()
	if len(hours) > 0 {
		v.Sections = append(v.Sections, a.hoursSection(hours, u))
	}

	if ds := f.days(7); len(ds) > 0 {
		v.Sections = append(v.Sections, a.forecastSection(ds, u))
	}

	v.Sections = append(v.Sections, a.airSection(ctx, c, freshness(refresh)))
	v.Sections = append(v.Sections, a.airOutlookSection(ctx, c, freshness(refresh)))
	v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{
		text("天气与空气质量：Open-Meteo；CAMS ENSEMBLE 数据提供方。花粉仅在欧洲花粉季提供，空白表示无数据，不代表浓度为零；信息仅供参考，不作医疗或用药建议。", wire.TextStyleCaption)}})

	tail := wire.Section{}
	if !rd.at.IsZero() {
		tail.Items = append(tail.Items, text("读于 "+rd.at.In(loc).Format("15:04")+"（10 分钟内不重读）", wire.TextStyleCaption))
	}
	tail.Items = append(tail.Items, button("刷新", "refresh"))
	v.Sections = append(v.Sections, tail)
	return v
}

// nowSection is the current weather: a big temperature with its icon, and the apparent temperature, humidity, wind
// and today's high and low beside it.
func (a *app) nowSection(c City, f forecast, u temperatureUnit, w windUnit) wire.Section {
	cur := f.Current
	desc, icon := wmoOptional(cur.WeatherCode)
	big := wire.Component{Type: wire.ComponentMetric, Label: "现在", Value: icon + " " + tempText(cur.Temperature, u), Detail: desc}

	stats := []wire.Stat{
		{Label: "体感", Value: tempText(cur.Apparent, u)},
		{Label: "湿度", Value: percentText(cur.Humidity)},
	}
	switch {
	case !cur.WindSpeed.OK:
		stats = append(stats, wire.Stat{Label: "风", Value: "未知"})
	case cur.WindSpeed.Value > 0:
		stats = append(stats, wire.Stat{Label: windDirLabel(cur.WindDirection), Value: fmtWind(cur.WindSpeed.Value, w)})
	default:
		stats = append(stats, wire.Stat{Label: "风", Value: "无风"})
	}
	if hi, lo, ok := f.todayHighLow(); ok {
		stats = append(stats, wire.Stat{Label: "今天", Value: fmtTemp(lo, u) + " / " + fmtTemp(hi, u)})
	}

	sec := wire.Section{Title: clip(c.Name, 1800), Items: []wire.Component{big}}
	sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentStatGrid, Stats: stats})
	return sec
}

// hoursSection is the next 24 hours as a temperature line.
func (a *app) hoursSection(hours []hourPoint, u temperatureUnit) wire.Section {
	pts := make([]wire.ChartPoint, 0, len(hours))
	for _, h := range hours {
		pts = append(pts, wire.ChartPoint{Label: h.at.Format("15") + "时", Value: u.temp(h.temp)})
	}
	chart := wire.Component{Type: wire.ComponentChart, Kind: wire.ChartKindLine, Unit: u.tempLabel(),
		Series: []wire.ChartSeries{{Name: "气温", Points: pts}}}
	return wire.Section{Title: "未来 24 小时", Items: []wire.Component{chart}}
}

// forecastSection is the 7-day forecast, one row a day.
func (a *app) forecastSection(ds []day, u temperatureUnit) wire.Section {
	sec := wire.Section{Title: "7 天预报"}
	for _, d := range ds {
		desc, icon := wmoOptional(d.code)
		title := d.weekday()
		if title == "" {
			if !d.date.IsZero() {
				title = d.date.Format("01-02")
			} else {
				title = "未知日期"
			}
		}
		row := wire.Component{Type: wire.ComponentRow,
			Title:    fmt.Sprintf("%s %s %s / %s", title, icon, tempText(d.max, u), tempText(d.min, u)),
			Subtitle: fmt.Sprintf("%s · 降水概率 %s", desc, percentText(d.precip))}
		sec.Items = append(sec.Items, row)
	}
	if len(sec.Items) == 0 {
		sec.Items = append(sec.Items, text("没有这几天的预报", wire.TextStyleSecondary))
	}
	return sec
}

// airSection is the air quality card; a failed read is its own note, not a page of zeros.
func (a *app) airSection(ctx context.Context, c City, fresh time.Duration) wire.Section {
	q, _, err := a.readAir(ctx, c, fresh)
	if err != nil {
		return wire.Section{Title: "空气质量", Items: []wire.Component{
			text(clip(oneLine(err.Error()), 1900), wire.TextStyleSecondary)}}
	}
	var stats []wire.Stat
	if q.Current.EuropeanAQI != nil {
		stats = append(stats, wire.Stat{Label: "空气质量指数", Value: fmt.Sprintf("%.0f · %s", *q.Current.EuropeanAQI, aqiLevel(*q.Current.EuropeanAQI))})
	}
	if q.Current.PM25 != nil {
		stats = append(stats, wire.Stat{Label: "PM2.5", Value: trimZero(*q.Current.PM25) + " µg/m³", Detail: pm25Level(*q.Current.PM25)})
	}
	if q.Current.PM10 != nil {
		stats = append(stats, wire.Stat{Label: "PM10", Value: trimZero(*q.Current.PM10) + " µg/m³"})
	}
	if len(stats) == 0 {
		return wire.Section{Title: "空气质量", Items: []wire.Component{
			text("这个城市没有空气质量数据", wire.TextStyleSecondary)}}
	}
	return wire.Section{Title: "空气质量", Items: []wire.Component{{Type: wire.ComponentStatGrid, Stats: stats}}}
}

// airOutlookSection keeps the optional hourly outlook separate from the existing current-air-quality card.
func (a *app) airOutlookSection(ctx context.Context, c City, fresh time.Duration) wire.Section {
	q, _, err := a.readAirOutlook(ctx, c, fresh)
	if err != nil {
		return wire.Section{Title: "花粉与空气质量趋势", Items: []wire.Component{
			text(clip(oneLine(err.Error()), 1900), wire.TextStyleSecondary)}}
	}
	hours := q.outlook()
	sec := wire.Section{Title: "花粉与空气质量趋势"}
	var pollenStats []wire.Stat
	if len(hours) > 0 {
		for _, item := range hours[0].Pollen {
			pollenStats = append(pollenStats, wire.Stat{Label: pollenLabel(item.Species), Value: trimZero(item.Value) + " grains/m³"})
		}
	}
	if len(pollenStats) > 0 {
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentStatGrid, Stats: pollenStats})
	} else {
		sec.Items = append(sec.Items, text("花粉仅在欧洲花粉季提供；当前没有花粉数据。无数据不等于浓度为零。", wire.TextStyleSecondary))
	}
	for _, point := range hours[:min(6, len(hours))] {
		var parts []string
		if point.AQI != nil {
			parts = append(parts, fmt.Sprintf("AQI %.0f（%s）", *point.AQI, aqiLevel(*point.AQI)))
		}
		if point.PM25 != nil {
			parts = append(parts, "PM2.5 "+trimZero(*point.PM25)+" µg/m³")
		}
		if point.PM10 != nil {
			parts = append(parts, "PM10 "+trimZero(*point.PM10)+" µg/m³")
		}
		if len(parts) == 0 {
			parts = append(parts, "暂无空气质量数据")
		}
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentRow, Title: point.At.Format("01-02 15:00"), Subtitle: strings.Join(parts, " · ")})
	}
	return sec
}

// badCityNames lists the unusable cities in words.
func badCityNames(bad []City) string {
	names := make([]string, 0, len(bad))
	for _, c := range bad {
		n := c.Name
		if strings.TrimSpace(n) == "" {
			n = "（没写名字）"
		}
		names = append(names, n)
	}
	return strings.Join(names, "、")
}
