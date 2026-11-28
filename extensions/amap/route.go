package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

// Route planning 2.0 (the LBS allowance): driving, walking, cycling, e-bike and public transport, written as distance
// and time, then what matters for the mode (tolls, lights, main roads and the taxi estimate for driving; the lines,
// stops, fare and walking for transport).

// stepsMost is how many turn-by-turn steps a route lists at most.
const stepsMost = 15

// waypointsMost is how many driving waypoints AMap's v5 driving API accepts.
const waypointsMost = 16

// driveStrategies are AMap's driving strategies for the tool's preferences.
var driveStrategies = map[string]string{
	"recommended":      "32",
	"fastest":          "38",
	"avoid_congestion": "33",
	"avoid_tolls":      "1",
	"avoid_highways":   "35",
}

var preferWords = map[string]string{
	"recommended":      "高德推荐",
	"fastest":          "最快",
	"avoid_congestion": "躲避拥堵",
	"avoid_tolls":      "不走收费路",
	"avoid_highways":   "不走高速",
}

var modeWords = map[string]string{"driving": "驾车", "walking": "步行", "cycling": "骑行", "ebike": "电动车", "transit": "公交地铁"}

var modePaths = map[string]string{
	"driving": "/v5/direction/driving",
	"walking": "/v5/direction/walking",
	"cycling": "/v5/direction/bicycling",
	"ebike":   "/v5/direction/electrobike",
	"transit": "/v5/direction/transit/integrated",
}

// end is one end of a route: the point given, or the fixed-place/name looked up preferring city.
func (a *app) end(ctx context.Context, text string, p point, given bool, city, which string) (place, error) {
	if given {
		name := text
		if name == "" {
			name = which
		}
		return place{name: name, at: p}, nil
	}
	return a.resolvePoint(ctx, text, city)
}

// resolveRouteStops resolves from, to, and waypoints together in a single consistent order so fixed places stay
// global while unconfigured names get disambiguated from the other endpoint's city. Both toolRoute and ui route preview
// use this helper so preview lookups always hit the cache.
func (a *app) resolveRouteStops(ctx context.Context, fromText string, fromAt point, fromGiven bool, toText string, toAt point, toGiven bool, rawWaypoints []string, initCity, mode string) (from, to place, viaPlaces []place, err error) {
	fromNeedsCity := !fromGiven && !a.isFixedOrCoord(fromText)
	toNeedsCity := !toGiven && !a.isFixedOrCoord(toText)

	city, knownCity := initCity, ""
	if fromGiven != toGiven {
		otherNeedsCity := (fromGiven && toNeedsCity) || (toGiven && fromNeedsCity)
		if mode == "transit" || otherNeedsCity {
			known := fromAt
			if toGiven {
				known = toAt
			}
			if c, cerr := a.cityCode(ctx, known); cerr == nil {
				knownCity = c
				if city == "" {
					city = c
				}
			}
		}
	}

	if !fromGiven && !toGiven && fromNeedsCity && !toNeedsCity && city == "" {
		to, err = a.end(ctx, toText, toAt, false, "", "终点")
		if err != nil {
			return place{}, place{}, nil, err
		}
		if to.citycode == "" {
			if c, cerr := a.cityCode(ctx, to.at); cerr == nil {
				to.citycode = c
			}
		}
		city = to.citycode
		from, err = a.end(ctx, fromText, fromAt, false, city, "起点")
		if err != nil {
			return place{}, place{}, nil, err
		}
	} else {
		from, err = a.end(ctx, fromText, fromAt, fromGiven, city, "起点")
		if err != nil {
			return place{}, place{}, nil, err
		}
		if fromGiven {
			from.citycode = knownCity
		}
		if city == "" && toNeedsCity {
			if from.citycode == "" {
				if c, cerr := a.cityCode(ctx, from.at); cerr == nil {
					from.citycode = c
				}
			}
			city = from.citycode
		}
		to, err = a.end(ctx, toText, toAt, toGiven, city, "终点")
		if err != nil {
			return place{}, place{}, nil, err
		}
		if toGiven {
			to.citycode = knownCity
		}
	}

	wpCity := city
	if wpCity == "" {
		wpCity = from.citycode
	}
	if wpCity == "" {
		wpCity = to.citycode
	}
	if wpCity == "" && len(rawWaypoints) > 0 {
		for _, w := range rawWaypoints {
			if !a.isFixedOrCoord(w) {
				if c, cerr := a.cityCode(ctx, from.at); cerr == nil {
					wpCity = c
				}
				break
			}
		}
	}
	for _, w := range rawWaypoints {
		pl, err := a.resolvePoint(ctx, w, wpCity)
		if err != nil {
			return place{}, place{}, nil, err
		}
		viaPlaces = append(viaPlaces, pl)
		if wpCity == "" && pl.citycode != "" {
			wpCity = pl.citycode
		}
	}
	return from, to, viaPlaces, nil
}

// label is a place as the answer names it: what the user called it, and where AMap put it when it was looked up.
func (p place) label() string {
	if p.where == "" || strings.Contains(p.name, p.where) {
		return p.name
	}
	return p.name + "（" + p.where + "）"
}

type plannedRoute struct {
	text    string
	head    string
	mode    string
	from    place
	to      place
	via     []place
	options []routeOption
}

func (a *app) toolRoute(ctx context.Context, in args) (wire.ConnectorCallResult, error) {
	plan, err := a.planRoute(ctx, in)
	if err != nil {
		return wire.ConnectorCallResult{}, err
	}
	res := wire.ConnectorCallResult{Text: plan.text}
	if mc := buildRouteMapComponent(
		"",
		fmt.Sprintf("%s → %s", plan.from.name, plan.to.name),
		routeAltText(plan),
		routeCaptionText(plan),
		plan.from,
		plan.to,
		plan.via,
		plan.mode,
		plan.options,
		false, // keep compact chat card clean; full screen expands route selector & list
	); mc != nil {
		res.Views = []wire.View{{
			Title:    clipFit(plan.head, 120),
			Sections: []wire.Section{{Items: []wire.Component{*mc}}},
		}}
	}
	return res, nil
}

func routeAltText(plan plannedRoute) string {
	if len(plan.options) > 0 && plan.options[0].summary != "" {
		return fmt.Sprintf("%s（%s）", plan.head, plan.options[0].summary)
	}
	return plan.head
}

func routeCaptionText(plan plannedRoute) string {
	if len(plan.options) == 0 {
		return ""
	}
	c := plan.options[0].summary
	if len(plan.options) > 1 {
		if c != "" {
			c += " · "
		}
		c += fmt.Sprintf("共 %d 个方案，点开可切换", len(plan.options))
	}
	return c
}

func (a *app) planRoute(ctx context.Context, in args) (plannedRoute, error) {
	mode := in.Mode
	if mode == "" {
		mode = "driving"
	}
	if modePaths[mode] == "" {
		return plannedRoute{}, fmt.Errorf("不认识出行方式 %q：driving、walking、cycling、ebike 或 transit", mode)
	}
	prefer := in.Prefer
	if prefer == "" {
		prefer = "recommended"
	}
	if driveStrategies[prefer] == "" {
		return plannedRoute{}, fmt.Errorf("不认识驾车偏好 %q", prefer)
	}
	var date, clock string
	if mode == "transit" && in.DepartAt != "" {
		t, err := time.ParseInLocation("2006-01-02 15:04", in.DepartAt, beijing)
		if err != nil {
			return plannedRoute{}, errors.New("depart_at 要写成「2026-11-16 08:30」这样（北京时间）")
		}
		date, clock = t.Format("2006-01-02"), fmt.Sprintf("%d-%02d", t.Hour(), t.Minute())
	}
	fromAt, fromGiven, err := pointArg(in.FromLatitude, in.FromLongitude, "起点")
	if err != nil {
		return plannedRoute{}, err
	}
	toAt, toGiven, err := pointArg(in.ToLatitude, in.ToLongitude, "终点")
	if err != nil {
		return plannedRoute{}, err
	}
	for _, e := range []struct {
		given      bool
		text, what string
		param      string
	}{{fromGiven, in.From, "起点", "from"}, {toGiven, in.To, "终点", "to"}} {
		if !e.given && e.text == "" {
			return plannedRoute{}, fmt.Errorf("要给%s：坐标（%s_latitude、%s_longitude），或地址、地名、常用地点名如「家」「公司」（%s）。用户说「从这儿」「我这儿」时先用 phone_location_current，不确定「家」「公司」是否已配置时先用 amap_places 或 list_places", e.what, e.param, e.param, e.param)
		}
	}
	var rawWaypoints []string
	for _, w := range in.Waypoints {
		if w = strings.TrimSpace(w); w != "" {
			rawWaypoints = append(rawWaypoints, w)
		}
	}
	if len(rawWaypoints) > 0 {
		if mode != "driving" {
			return plannedRoute{}, errors.New("途经点（waypoints）目前只支持驾车（driving）路线")
		}
		if len(rawWaypoints) > waypointsMost {
			return plannedRoute{}, fmt.Errorf("途经点最多支持 %d 个", waypointsMost)
		}
	}
	from, to, viaPlaces, err := a.resolveRouteStops(ctx, in.From, fromAt, fromGiven, in.To, toAt, toGiven, rawWaypoints, in.City, mode)
	if err != nil {
		return plannedRoute{}, err
	}
	viaCoords := make([]string, len(viaPlaces))
	for i, pl := range viaPlaces {
		viaCoords[i] = amapLocation(pl.at)
	}
	if len(viaPlaces) == 0 && metres(from.at, to.at) < 20 {
		return plannedRoute{
			text: fmt.Sprintf("%s和%s几乎在同一个地方，不用规划路线。", from.label(), to.label()),
			mode: mode,
			from: from,
			to:   to,
		}, nil
	}
	alts := limitOr(in.Alternatives, 1, 3)
	q := url.Values{"origin": {amapLocation(from.at)}, "destination": {amapLocation(to.at)}, "show_fields": {"cost,polyline"}}
	ttl := routeFor
	switch mode {
	case "driving":
		q.Set("strategy", driveStrategies[prefer])
		if len(viaCoords) > 0 {
			q.Set("waypoints", strings.Join(viaCoords, ";"))
		}
		ttl = drivingFor
	case "walking", "cycling", "ebike":
		if alts > 1 {
			q.Set("alternative_route", strconv.Itoa(alts))
		}
	case "transit":
		if from.citycode == "" {
			if from.citycode, err = a.cityCode(ctx, from.at); err != nil {
				return plannedRoute{}, err
			}
		}
		if to.citycode == "" {
			if to.citycode, err = a.cityCode(ctx, to.at); err != nil {
				return plannedRoute{}, err
			}
		}
		q.Set("city1", from.citycode)
		q.Set("city2", to.citycode)
		q.Set("AlternativeRoute", strconv.Itoa(alts))
		if date != "" {
			q.Set("date", date)
			q.Set("time", clock)
		}
	}
	body, err := a.currentClient().get(ctx, groupLBS, modePaths[mode], q, ttl)
	if err != nil {
		return plannedRoute{}, err
	}
	head := fmt.Sprintf("%s %s → %s", modeWords[mode], from.label(), to.label())
	if len(viaPlaces) > 0 {
		viaLabels := make([]string, len(viaPlaces))
		for i, vp := range viaPlaces {
			viaLabels[i] = vp.label()
		}
		head = fmt.Sprintf("%s %s → 经 %s → %s", modeWords[mode], from.label(), strings.Join(viaLabels, "、"), to.label())
	}
	if mode == "driving" {
		head += "（" + preferWords[prefer] + "）"
	}
	out := plannedRoute{head: head, mode: mode, from: from, to: to, via: viaPlaces}
	if mode == "transit" {
		out.text, out.options, err = transitPlan(body, head, in.DepartAt, alts)
	} else {
		out.text, out.options, err = pathPlan(body, head, mode, alts, in.Steps)
	}
	if err != nil {
		return plannedRoute{}, err
	}
	return out, nil
}

// pathPlan formats a driving, walking, cycling or e-bike text answer alongside decoded map polylines.
func pathPlan(body []byte, head, mode string, alts int, withSteps bool) (string, []routeOption, error) {
	var ans pathAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return "", nil, errBadAnswer
	}
	route := ans.Route.V
	if len(route.Paths) == 0 {
		return head + "：高德没算出路线。", nil, nil
	}
	n := min(alts, len(route.Paths))
	options := make([]routeOption, 0, n)
	var b strings.Builder
	b.WriteString(head + "：")
	for i, p := range route.Paths {
		if i == alts {
			break
		}
		if alts > 1 && len(route.Paths) > 1 {
			fmt.Fprintf(&b, "\n方案 %d：", i+1)
		} else {
			b.WriteString("\n")
		}
		var parts []string
		var summaryParts []string
		var distM, durS *int
		if d, ok := p.Distance.num(); ok {
			parts = append(parts, distanceWords(d))
			summaryParts = append(summaryParts, distanceWords(d))
			if d >= 0 && d <= 100000000 {
				distM = intPtr(int(math.Round(d)))
			}
		}
		c := p.Cost.V
		if s, ok := c.Duration.num(); ok {
			parts = append(parts, "约 "+durationWords(s))
			summaryParts = append([]string{"约 " + durationWords(s)}, summaryParts...)
			if s >= 0 && s <= 100000000 {
				durS = intPtr(int(math.Round(s)))
			}
		} else if s, ok := p.Duration.num(); ok {
			parts = append(parts, "约 "+durationWords(s))
			summaryParts = append([]string{"约 " + durationWords(s)}, summaryParts...)
			if s >= 0 && s <= 100000000 {
				durS = intPtr(int(math.Round(s)))
			}
		}
		if mode == "driving" {
			if n, ok := c.TrafficLights.num(); ok && n > 0 {
				parts = append(parts, fmt.Sprintf("红绿灯 %.0f 个", n))
				summaryParts = append(summaryParts, fmt.Sprintf("红绿灯 %.0f 个", n))
			}
			if t, ok := c.Tolls.num(); ok && t > 0 {
				parts = append(parts, "过路费约 ¥"+money(t))
				summaryParts = append(summaryParts, "过路费约 ¥"+money(t))
			} else if ok {
				parts = append(parts, "无过路费")
			}
			if p.Restriction.String() == "1" {
				parts = append(parts, "有避不开的限行")
				summaryParts = append(summaryParts, "有限行")
			}
		}
		if t, ok := c.Taxi.num(); ok && t > 0 {
			parts = append(parts, "打车约 ¥"+money(t))
		}
		b.WriteString(strings.Join(parts, "，"))
		if roads := mainRoads(p.Steps); mode == "driving" && len(roads) > 0 {
			b.WriteString("；主要走 " + strings.Join(roads, "、"))
		}
		if withSteps {
			for j, s := range p.Steps {
				if j == stepsMost {
					fmt.Fprintf(&b, "\n  （后面还有 %d 段）", len(p.Steps)-stepsMost)
					break
				}
				fmt.Fprintf(&b, "\n  %d. %s", j+1, s.Instruction)
			}
		}

		var pts []point
		for _, s := range p.Steps {
			pts = appendPoints(pts, decodePolyline(s.Polyline))
		}
		label := modeWords[mode]
		if n > 1 {
			label = fmt.Sprintf("方案 %d", i+1)
		}
		options = append(options, routeOption{
			id:              fmt.Sprintf("r-%d", i+1),
			label:           label,
			distanceMeters:  distM,
			durationSeconds: durS,
			summary:         strings.Join(summaryParts, " · "),
			points:          pts,
		})
	}
	if t, ok := route.TaxiCost.num(); ok && t > 0 && mode == "driving" {
		b.WriteString("\n打车约 ¥" + money(t) + "（高德估算）")
		if len(options) > 0 && options[0].summary != "" {
			options[0].summary += " · 打车约 ¥" + money(t)
		}
	}
	return b.String(), options, nil
}

// mainRoads are the three named roads a route spends longest on, in the order it meets them.
func mainRoads(steps []step) []string {
	length := map[string]float64{}
	var order []string
	for _, s := range steps {
		name := s.RoadName.String()
		if name == "" {
			continue
		}
		if _, ok := length[name]; !ok {
			order = append(order, name)
		}
		d, _ := s.StepDistance.num()
		length[name] += d
	}
	if len(order) <= 3 {
		return order
	}
	top := append([]string(nil), order...)
	for i := range top {
		for j := i + 1; j < len(top); j++ {
			if length[top[j]] > length[top[i]] {
				top[i], top[j] = top[j], top[i]
			}
		}
	}
	keep := map[string]bool{top[0]: true, top[1]: true, top[2]: true}
	var out []string
	for _, name := range order {
		if keep[name] {
			out = append(out, name)
		}
	}
	return out
}

// transitPlan writes a public transport answer alongside decoded segment polylines and transfer stops.
func transitPlan(body []byte, head, departAt string, alts int) (string, []routeOption, error) {
	var ans transitAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return "", nil, errBadAnswer
	}
	route := ans.Route.V
	if departAt != "" {
		head += "（" + departAt + " 出发）"
	}
	if len(route.Transits) == 0 {
		return head + "：高德没找到公交地铁方案，可以试试驾车或步行。", nil, nil
	}
	n := min(alts, len(route.Transits))
	options := make([]routeOption, 0, n)
	var b strings.Builder
	b.WriteString(head + "：")
	for i, t := range route.Transits {
		if i == alts {
			break
		}
		var parts []string
		var summaryParts []string
		var distM, durS *int
		if d, ok := t.Distance.num(); ok && d >= 0 && d <= 100000000 {
			distM = intPtr(int(math.Round(d)))
		}
		if s, ok := t.Cost.V.Duration.num(); ok {
			parts = append(parts, "约 "+durationWords(s))
			summaryParts = append(summaryParts, "约 "+durationWords(s))
			if s >= 0 && s <= 100000000 {
				durS = intPtr(int(math.Round(s)))
			}
		}
		if f, ok := t.Cost.V.TransitFee.num(); ok && f > 0 {
			parts = append(parts, "票价 ¥"+money(f))
			summaryParts = append(summaryParts, "¥"+money(f))
		}
		if w, ok := t.WalkingDistance.num(); ok && w > 0 {
			parts = append(parts, "步行共 "+distanceWords(w))
			summaryParts = append(summaryParts, "步行 "+distanceWords(w))
		}
		if t.NightFlag.String() == "1" {
			parts = append(parts, "夜班车")
		}
		fmt.Fprintf(&b, "\n方案 %d：%s\n  %s", i+1, strings.Join(parts, "，"), strings.Join(legs(t.Segments), " → "))

		pts, stops := transitGeometry(t.Segments)
		label := "公交地铁"
		if n > 1 {
			label = fmt.Sprintf("方案 %d", i+1)
		}
		options = append(options, routeOption{
			id:              fmt.Sprintf("r-%d", i+1),
			label:           label,
			distanceMeters:  distM,
			durationSeconds: durS,
			summary:         strings.Join(summaryParts, " · "),
			points:          pts,
			stops:           stops,
		})
	}
	if f, ok := route.Cost.V.TaxiFee.num(); ok && f > 0 {
		b.WriteString("\n打车约 ¥" + money(f) + "（高德估算）")
	}
	return b.String(), options, nil
}

func transitGeometry(segs []segment) ([]point, []routeStop) {
	var pts []point
	var stops []routeStop
	for idx, s := range segs {
		if w := s.Walking.V; w.Polyline.String() != "" || w.Origin.String() != "" || w.Destination.String() != "" {
			if segPts := decodePolyline(w.Polyline); len(segPts) >= 2 {
				pts = appendPoints(pts, segPts)
			} else {
				if p, ok := parseAMapLocation(w.Origin.String()); ok {
					pts = appendPoints(pts, []point{p})
				}
				if p, ok := parseAMapLocation(w.Destination.String()); ok {
					pts = appendPoints(pts, []point{p})
				}
			}
		}
		if lines := s.Bus.V.Buslines; len(lines) > 0 {
			l := lines[0]
			shortLine := lineName(l.Name.String())
			depPt, hasDep := parseAMapLocation(l.DepartureStop.V.Location.String())
			arrPt, hasArr := parseAMapLocation(l.ArrivalStop.V.Location.String())
			if segPts := decodePolyline(l.Polyline); len(segPts) >= 2 {
				pts = appendPoints(pts, segPts)
			} else {
				if hasDep {
					pts = appendPoints(pts, []point{depPt})
				}
				if hasArr {
					pts = appendPoints(pts, []point{arrPt})
				}
			}
			if hasDep {
				if name := l.DepartureStop.V.Name.String(); name != "" {
					stops = append(stops, routeStop{
						id:       fmt.Sprintf("bus-dep-%d", idx+1),
						name:     name,
						subtitle: "乘车 " + shortLine,
						at:       depPt,
					})
				}
			}
			if hasArr {
				if name := l.ArrivalStop.V.Name.String(); name != "" {
					stops = append(stops, routeStop{
						id:       fmt.Sprintf("bus-arr-%d", idx+1),
						name:     name,
						subtitle: "下车 " + shortLine,
						at:       arrPt,
					})
				}
			}
		}
		if r := s.Railway.V; r.Name.String() != "" || r.Trip.String() != "" {
			train := r.Trip.String()
			if train == "" {
				train = r.Name.String()
			}
			if p, ok := parseAMapLocation(r.DepartureStop.V.Location.String()); ok {
				pts = appendPoints(pts, []point{p})
				if name := r.DepartureStop.V.Name.String(); name != "" {
					stops = append(stops, routeStop{
						id:       fmt.Sprintf("rail-dep-%d", idx+1),
						name:     name,
						subtitle: "乘火车 " + train,
						at:       p,
					})
				}
			}
			if p, ok := parseAMapLocation(r.ArrivalStop.V.Location.String()); ok {
				pts = appendPoints(pts, []point{p})
				if name := r.ArrivalStop.V.Name.String(); name != "" {
					stops = append(stops, routeStop{
						id:       fmt.Sprintf("rail-arr-%d", idx+1),
						name:     name,
						subtitle: "到达 " + train,
						at:       p,
					})
				}
			}
		}
		if tx := s.Taxi.V; tx.Polyline.String() != "" || tx.StartPoint.String() != "" || tx.EndPoint.String() != "" {
			if segPts := decodePolyline(tx.Polyline); len(segPts) >= 2 {
				pts = appendPoints(pts, segPts)
			} else {
				if p, ok := parseAMapLocation(tx.StartPoint.String()); ok {
					pts = appendPoints(pts, []point{p})
				}
				if p, ok := parseAMapLocation(tx.EndPoint.String()); ok {
					pts = appendPoints(pts, []point{p})
				}
			}
		}
	}
	return pts, stops
}

// lineName writes a line with the way it runs: AMap names it "line(start--end)", so 地铁4号线大兴线(安河桥北--天宫院) is
// 地铁4号线大兴线（往天宫院）; an end may hold parentheses of its own (唯亭(苏州新区火车站)), and a loop keeps only its name.
func lineName(s string) string {
	if !strings.HasSuffix(s, ")") && !strings.HasSuffix(s, "）") {
		return s
	}
	for i, r := range s {
		if r != '(' && r != '（' {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimSuffix(s[i+len(string(r)):], ")"), "）")
		start, end, ok := strings.Cut(inner, "--")
		if !ok {
			continue
		}
		name := strings.TrimSpace(s[:i])
		if start == end || end == "" {
			return name
		}
		return name + "（往" + end + "）"
	}
	return s
}

// legs writes a plan's segments in order: the walk to a stop, then the ride.
func legs(segs []segment) []string {
	var out []string
	for _, s := range segs {
		if d, ok := s.Walking.V.Distance.num(); ok && d > 0 {
			out = append(out, "步行 "+distanceWords(d))
		}
		if lines := s.Bus.V.Buslines; len(lines) > 0 {
			l := lines[0]
			name := lineName(l.Name.String())
			if !strings.HasSuffix(name, "）") {
				name += " "
			}
			leg := fmt.Sprintf("%s%s→%s", name, l.DepartureStop.V.Name, l.ArrivalStop.V.Name)
			if n, ok := l.ViaNum.num(); ok {
				leg += fmt.Sprintf("（%.0f 站）", n+1)
			}
			var also []string
			for _, other := range lines[1:] {
				if len(also) < 2 {
					also = append(also, lineName(other.Name.String()))
				}
			}
			if len(also) > 0 {
				leg += "（也可坐 " + strings.Join(also, "、") + "）"
			}
			out = append(out, leg)
		}
		if r := s.Railway.V; r.Name.String() != "" || r.Trip.String() != "" {
			name := r.Trip.String()
			if name == "" {
				name = r.Name.String()
			}
			leg := fmt.Sprintf("火车 %s %s", name, r.DepartureStop.V.Name)
			if t := clockWords(r.DepartureStop.V.Time.String()); t != "" {
				leg += " " + t
			}
			leg += "→" + r.ArrivalStop.V.Name.String()
			if t := clockWords(r.ArrivalStop.V.Time.String()); t != "" {
				leg += " " + t
			}
			out = append(out, leg)
		}
		if d, ok := s.Taxi.V.Distance.num(); ok && d > 0 {
			leg := "打车 " + distanceWords(d)
			if p, ok := s.Taxi.V.Price.num(); ok && p > 0 {
				leg += "（约 ¥" + money(p) + "）"
			}
			out = append(out, leg)
		}
	}
	return out
}

// clockWords writes AMap's "0800" or "08:00" as 08:00.
func clockWords(s string) string {
	s = strings.ReplaceAll(s, ":", "")
	if len(s) != 4 {
		return ""
	}
	if _, err := strconv.Atoi(s); err != nil {
		return ""
	}
	return s[:2] + ":" + s[2:]
}

// durationWords writes seconds as minutes, or hours and minutes.
func durationWords(sec float64) string {
	m := int(math.Round(sec / 60))
	switch {
	case m < 1:
		return "1 分钟"
	case m < 60:
		return fmt.Sprintf("%d 分钟", m)
	case m%60 == 0:
		return fmt.Sprintf("%d 小时", m/60)
	}
	return fmt.Sprintf("%d 小时 %d 分钟", m/60, m%60)
}

// money writes yuan without trailing zeros.
func money(f float64) string { return strconv.FormatFloat(math.Round(f*10)/10, 'f', -1, 64) }
