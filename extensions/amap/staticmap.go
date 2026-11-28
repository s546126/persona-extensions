package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// Static map (/v3/staticmap, the LBS allowance): renders a PNG map centered on a point, markers and/or a polyline
// path, and returns it as a ConnectorToolImage alongside a standalone text summary (for hubs without tool_images).

const (
	staticMapMarkersMost = 10
	staticMapPathMost    = 20
	staticMapMaxBytes    = 1 << 20 // Persona ConnectorToolImage limit is 1 MiB per image
)

func (a *app) toolStaticMap(ctx context.Context, in args) (wire.ConnectorCallResult, error) {
	center, hasCenter, err := pointArg(in.Latitude, in.Longitude, "中心点")
	if err != nil {
		return wire.ConnectorCallResult{}, err
	}
	var rawMarkers []string
	for _, m := range in.Markers {
		if m = strings.TrimSpace(m); m != "" {
			rawMarkers = append(rawMarkers, m)
		}
	}
	if len(rawMarkers) > staticMapMarkersMost {
		return wire.ConnectorCallResult{}, fmt.Errorf("markers 最多支持 %d 个标注点", staticMapMarkersMost)
	}
	var rawPath []string
	for _, p := range in.Path {
		if p = strings.TrimSpace(p); p != "" {
			rawPath = append(rawPath, p)
		}
	}
	if len(rawPath) == 1 {
		return wire.ConnectorCallResult{}, errors.New("path 至少要给 2 个点才能连成线")
	}
	if len(rawPath) > staticMapPathMost {
		return wire.ConnectorCallResult{}, fmt.Errorf("path 最多支持 %d 个点", staticMapPathMost)
	}
	if !hasCenter && in.Place == "" && len(rawMarkers) == 0 && len(rawPath) == 0 {
		return wire.ConnectorCallResult{}, errors.New("要给中心点（latitude/longitude 或 place，也可填常用地点名如「家」）、标注点（markers）或路线点（path）中的至少一项")
	}

	width := 600
	if in.Width > 0 {
		width = max(100, min(in.Width, 1024))
	}
	height := 400
	if in.Height > 0 {
		height = max(100, min(in.Height, 1024))
	}

	city := in.City
	needPointCity := false
	for _, m := range rawMarkers {
		if !a.isFixedOrCoord(m) {
			needPointCity = true
			break
		}
	}
	if !needPointCity {
		for _, p := range rawPath {
			if !a.isFixedOrCoord(p) {
				needPointCity = true
				break
			}
		}
	}
	var centerPlace place
	if hasCenter {
		name := in.Place
		if name == "" {
			name = center.String()
		}
		centerPlace = place{name: name, at: center}
		if city == "" && needPointCity {
			if c, err := a.cityCode(ctx, center); err == nil {
				city = c
			}
		}
	} else if in.Place != "" {
		pl, err := a.resolvePoint(ctx, in.Place, city)
		if err != nil {
			return wire.ConnectorCallResult{}, err
		}
		centerPlace = pl
		hasCenter = true
		if city == "" {
			city = pl.citycode
		}
		if city == "" && needPointCity {
			if c, err := a.cityCode(ctx, pl.at); err == nil {
				city = c
			}
		}
	}

	markers := make([]place, 0, len(rawMarkers))
	for _, m := range rawMarkers {
		pl, err := a.resolvePoint(ctx, m, city)
		if err != nil {
			return wire.ConnectorCallResult{}, err
		}
		markers = append(markers, pl)
		if city == "" && pl.citycode != "" {
			city = pl.citycode
		}
	}

	pathPts := make([]place, 0, len(rawPath))
	for _, p := range rawPath {
		pl, err := a.resolvePoint(ctx, p, city)
		if err != nil {
			return wire.ConnectorCallResult{}, err
		}
		pathPts = append(pathPts, pl)
		if city == "" && pl.citycode != "" {
			city = pl.citycode
		}
	}

	q := url.Values{
		"size":  {fmt.Sprintf("%d*%d", width, height)},
		"scale": {"2"},
	}
	if hasCenter {
		q.Set("location", amapLocation(centerPlace.at))
	}
	zoom := 0
	if in.Zoom != nil {
		zoom = max(1, min(*in.Zoom, 17))
		q.Set("zoom", strconv.Itoa(zoom))
	} else if len(markers) == 0 && len(pathPts) == 0 {
		// AMap requires zoom when neither markers nor paths are given for auto-fitting.
		zoom = 14
		q.Set("zoom", "14")
	}
	if len(markers) > 0 {
		parts := make([]string, len(markers))
		for i, m := range markers {
			label := string(rune('A' + i))
			parts[i] = fmt.Sprintf("mid,0xE53935,%s:%s", label, amapLocation(m.at))
		}
		q.Set("markers", strings.Join(parts, "|"))
	}
	if len(pathPts) > 0 {
		coords := make([]string, len(pathPts))
		for i, p := range pathPts {
			coords[i] = amapLocation(p.at)
		}
		q.Set("paths", "5,0x1976D2,1,,:"+strings.Join(coords, ";"))
	}
	if in.Traffic {
		q.Set("traffic", "1")
	}

	ttl := routeFor
	if in.Traffic {
		ttl = drivingFor
	}
	cl := a.currentClient()
	img, err := cl.getImage(ctx, groupLBS, "/v3/staticmap", q, ttl)
	if err != nil {
		return wire.ConnectorCallResult{}, err
	}
	if len(img) > staticMapMaxBytes {
		q1 := url.Values{}
		for k, v := range q {
			q1[k] = v
		}
		q1.Set("scale", "1")
		img1, err := cl.getImage(ctx, groupLBS, "/v3/staticmap", q1, ttl)
		if err != nil {
			return wire.ConnectorCallResult{}, err
		}
		if len(img1) > staticMapMaxBytes {
			return wire.ConnectorCallResult{}, errors.New("高德生成的静态地图超过 1 MB，请把 width/height 调小再试")
		}
		img = img1
	}

	var b strings.Builder
	b.WriteString("高德静态地图")
	var meta []string
	meta = append(meta, fmt.Sprintf("%d×%d", width, height))
	if zoom > 0 {
		meta = append(meta, fmt.Sprintf("缩放 %d 级", zoom))
	}
	if in.Traffic {
		meta = append(meta, "含实时路况")
	}
	fmt.Fprintf(&b, "（%s）：", strings.Join(meta, "，"))
	if hasCenter {
		fmt.Fprintf(&b, "\n中心：%s · %s", centerPlace.label(), centerPlace.at.String())
	}
	if len(markers) > 0 {
		b.WriteString("\n标注：")
		for i, m := range markers {
			fmt.Fprintf(&b, "\n- %c. %s · %s", rune('A'+i), m.label(), m.at.String())
		}
	}
	if len(pathPts) > 0 {
		names := make([]string, len(pathPts))
		for i, p := range pathPts {
			names[i] = p.label()
		}
		fmt.Fprintf(&b, "\n连线：%s", strings.Join(names, " → "))
	}

	caption := "高德静态地图"
	switch {
	case hasCenter:
		caption = "高德地图：" + centerPlace.label()
	case len(markers) > 0:
		caption = "高德地图：" + markers[0].label()
		if len(markers) > 1 {
			caption += fmt.Sprintf(" 等 %d 处", len(markers))
		}
	case len(pathPts) > 0:
		caption = fmt.Sprintf("高德地图：%s → %s", pathPts[0].label(), pathPts[len(pathPts)-1].label())
	}
	return wire.ConnectorCallResult{
		Text:   b.String(),
		Images: []wire.ConnectorToolImage{ext.ToolImage(img, caption)},
	}, nil
}
