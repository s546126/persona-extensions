package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Distance measurement (/v3/distance, the LBS allowance): compare up to 20 origins against one destination by driving,
// walking or straight-line distance.

const distanceOriginsMost = 20

var distanceModes = map[string]struct {
	typ   string
	label string
}{
	"driving":  {"1", "驾车"},
	"walking":  {"3", "步行"},
	"straight": {"0", "直线"},
}

func (a *app) toolDistance(ctx context.Context, in args) (string, error) {
	mode := in.Mode
	if mode == "" {
		mode = "driving"
	}
	spec, ok := distanceModes[mode]
	if !ok {
		return "", fmt.Errorf("不认识距离测量方式 %q：driving、walking 或 straight", mode)
	}
	sortBy := in.Sort
	if sortBy == "" {
		sortBy = "input"
	}
	if sortBy != "input" && sortBy != "distance" && sortBy != "duration" {
		return "", fmt.Errorf("不认识排序方式 %q：input、distance 或 duration", sortBy)
	}
	var rawOrigins []string
	for _, o := range in.Origins {
		if o = strings.TrimSpace(o); o != "" {
			rawOrigins = append(rawOrigins, o)
		}
	}
	if len(rawOrigins) == 0 {
		return "", errors.New("要给 origins：至少一个起点（地名、地址、常用地点名如「家」，或 WGS-84「纬度,经度」）")
	}
	if len(rawOrigins) > distanceOriginsMost {
		return "", fmt.Errorf("origins 一次最多支持 %d 个起点", distanceOriginsMost)
	}
	destAt, destGiven, err := pointArg(in.DestinationLatitude, in.DestinationLongitude, "终点")
	if err != nil {
		return "", err
	}
	if !destGiven && in.Destination == "" {
		return "", errors.New("要给终点：destination（地名、地址、常用地点名如「公司」，或 WGS-84「纬度,经度」），或 destination_latitude 和 destination_longitude")
	}
	city := in.City
	var dest place
	anyOriginNeedsCity := false
	for _, o := range rawOrigins {
		if !a.isFixedOrCoord(o) {
			anyOriginNeedsCity = true
			break
		}
	}
	if destGiven {
		name := in.Destination
		if name == "" {
			name = destAt.String()
		}
		dest = place{name: name, at: destAt}
		if city == "" && anyOriginNeedsCity {
			if c, err := a.cityCode(ctx, destAt); err == nil {
				city = c
			}
		}
	} else {
		dest, err = a.resolvePoint(ctx, in.Destination, city)
		if err != nil {
			return "", err
		}
		if city == "" {
			city = dest.citycode
		}
		if city == "" && anyOriginNeedsCity {
			if c, err := a.cityCode(ctx, dest.at); err == nil {
				city = c
			}
		}
	}

	origins := make([]place, 0, len(rawOrigins))
	originCoords := make([]string, 0, len(rawOrigins))
	for _, o := range rawOrigins {
		pl, err := a.resolvePoint(ctx, o, city)
		if err != nil {
			return "", err
		}
		origins = append(origins, pl)
		originCoords = append(originCoords, amapLocation(pl.at))
		if city == "" && pl.citycode != "" {
			city = pl.citycode
		}
	}

	q := url.Values{
		"origins":     {strings.Join(originCoords, "|")},
		"destination": {amapLocation(dest.at)},
		"type":        {spec.typ},
	}
	ttl := routeFor
	switch mode {
	case "driving":
		ttl = drivingFor
	case "straight":
		ttl = geoFor
	}
	body, err := a.currentClient().get(ctx, groupLBS, "/v3/distance", q, ttl)
	if err != nil {
		return "", err
	}
	var ans distanceAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return "", errBadAnswer
	}
	if len(ans.Results) == 0 {
		return fmt.Sprintf("高德没算出到 %s 的%s距离。", dest.label(), spec.label), nil
	}

	type row struct {
		idx      int
		label    string
		dist     float64
		hasDist  bool
		dur      float64
		hasDur   bool
		failInfo string
	}
	rows := make([]row, len(origins))
	for i, o := range origins {
		rows[i] = row{idx: i, label: o.label(), dist: math.Inf(1), dur: math.Inf(1)}
	}
	for _, item := range ans.Results {
		id, err := strconv.Atoi(item.OriginID.String())
		if err != nil || id < 1 || id > len(rows) {
			continue
		}
		r := &rows[id-1]
		if info := item.Info.String(); info != "" && item.Code.String() != "" && item.Code.String() != "0" {
			r.failInfo = info
			continue
		}
		if d, ok := item.Distance.num(); ok {
			r.dist, r.hasDist = d, true
		}
		if s, ok := item.Duration.num(); ok && mode != "straight" {
			r.dur, r.hasDur = s, true
		}
	}

	switch sortBy {
	case "distance":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].dist < rows[j].dist })
	case "duration":
		sort.SliceStable(rows, func(i, j int) bool {
			if mode == "straight" {
				return rows[i].dist < rows[j].dist
			}
			if rows[i].dur != rows[j].dur {
				return rows[i].dur < rows[j].dur
			}
			return rows[i].dist < rows[j].dist
		})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "高德%s距离测量（到 %s）：", spec.label, dest.label())
	for i, r := range rows {
		if r.failInfo != "" {
			fmt.Fprintf(&b, "\n%d. %s：无法计算（%s）", i+1, r.label, r.failInfo)
			continue
		}
		var parts []string
		if r.hasDist {
			parts = append(parts, distanceWords(r.dist))
		}
		if r.hasDur {
			parts = append(parts, "约 "+durationWords(r.dur))
		}
		if len(parts) == 0 {
			parts = append(parts, "高德未返回结果")
		}
		fmt.Fprintf(&b, "\n%d. %s：%s", i+1, r.label, strings.Join(parts, "，"))
	}
	return b.String(), nil
}
