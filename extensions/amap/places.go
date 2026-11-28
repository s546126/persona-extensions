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
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

// Places: keyword search and around search (POI 2.0, the search allowance), geocoding and reverse geocoding (the LBS
// allowance), and how the tools write them.

// How long answers are reused.
const (
	geoFor     = 30 * 24 * time.Hour // addresses and coordinates hardly move
	searchFor  = 15 * time.Minute
	liveFor    = 20 * time.Minute
	castFor    = time.Hour
	drivingFor = 2 * time.Minute // live traffic
	routeFor   = 15 * time.Minute
)

// coarseLevels are geocoding matches too rough to stand for a place someone named (a whole city for 国贸).
var coarseLevels = map[string]bool{"国家": true, "省": true, "市": true, "区县": true, "开发区": true, "乡镇": true, "未知": true, "": true}

// errNotFound says AMap found nothing for a name.
type errNotFound struct{ what string }

func (e errNotFound) Error() string {
	return "高德没找到「" + e.what + "」，换个更完整的说法试试"
}

// geocode looks an address up (LBS allowance).
func (a *app) geocode(ctx context.Context, address, city string) ([]geocode, error) {
	q := url.Values{"address": {address}}
	if city != "" {
		q.Set("city", city)
	}
	body, err := a.currentClient().get(ctx, groupLBS, "/v3/geocode/geo", q, geoFor)
	if err != nil {
		return nil, err
	}
	var ans geoAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return nil, errBadAnswer
	}
	return ans.Geocodes, nil
}

// search is a keyword search (search allowance).
func (a *app) search(ctx context.Context, keywords, city string, cityOnly bool, limit int) ([]poi, error) {
	q := url.Values{"keywords": {keywords}, "page_size": {strconv.Itoa(limit)}, "show_fields": {"business"}}
	if city != "" {
		q.Set("region", city)
		if cityOnly {
			q.Set("city_limit", "true")
		}
	}
	body, err := a.currentClient().get(ctx, groupSearch, "/v5/place/text", q, searchFor)
	if err != nil {
		return nil, err
	}
	var ans poiAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return nil, errBadAnswer
	}
	return ans.Pois, nil
}

// place is somewhere a tool resolved: its name, WGS-84 point and, when known, its city/ad codes and where AMap put
// it (city and district, for the answer, so a same-named place in another city shows).
type place struct {
	name     string
	at       point
	citycode string
	adcode   string
	where    string
}

// isFixedOrCoord reports whether s is a literal WGS-84 point or matches a configured/saved fixed place.
func (a *app) isFixedOrCoord(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if _, ok := parseLatLng(s); ok {
		return true
	}
	_, ok := a.matchFixedPlace(s)
	return ok
}

// resolveFixedPlace resolves a configured or UI-saved place name/alias without letting another endpoint's city hint
// constrain a global anchor.
func (a *app) resolveFixedPlace(ctx context.Context, text string) (place, bool, error) {
	fp, ok := a.matchFixedPlace(text)
	if !ok {
		return place{}, false, nil
	}
	if fp.HasCoord {
		where := fp.Target
		if strings.EqualFold(strings.TrimSpace(where), strings.TrimSpace(fp.Name)) {
			where = ""
		}
		return place{name: fp.Name, at: fp.coord(), where: where}, true, nil
	}
	target := fp.Target
	if target == "" {
		target = fp.Name
	}
	pl, err := a.locate(ctx, target, "")
	if err != nil {
		return place{}, true, fmt.Errorf("常用地点「%s」（%s）解析失败：%w", fp.Name, target, err)
	}
	pl.name = fp.Name
	if pl.where == "" && target != "" && !strings.EqualFold(target, fp.Name) {
		pl.where = target
	}
	return pl, true, nil
}

// addressLike is text written as an address (a number with a road, lane or house word) rather than a place's name.
func addressLike(text string) bool {
	return strings.ContainsAny(text, "0123456789０１２３４５６７８９") && strings.ContainsAny(text, "路街道巷弄号村镇栋幢室")
}

// resolvePoint turns WGS-84 "lat,lng", a configured/saved fixed place, or a place name/address into a place, preferring
// city for unconfigured name lookups.
func (a *app) resolvePoint(ctx context.Context, text, city string) (place, error) {
	text = strings.TrimSpace(text)
	if p, ok := parseLatLng(text); ok {
		return place{name: p.String(), at: p}, nil
	}
	if pl, matched, err := a.resolveFixedPlace(ctx, text); matched {
		return pl, err
	}
	pl, err := a.locate(ctx, text, city)
	if err != nil {
		return place{}, err
	}
	pl.name = text
	return pl, nil
}

// locate turns a name or an address into a place, preferring city (a name, citycode or adcode; may be empty).
// An address is geocoded (the larger allowance); a place's name is searched for, because geocoding matches names
// poorly (东方明珠 without a city went to a shop of that name in another province). Each falls back on the other,
// and a lookup's answer is kept for a month.
func (a *app) locate(ctx context.Context, text, city string) (place, error) {
	if addressLike(text) {
		pl, rough, gerr := a.locateByGeocode(ctx, text, city)
		if gerr == nil && !rough {
			return pl, nil
		}
		hint := city
		if hint == "" {
			hint = pl.citycode
		}
		if found, serr := a.locateBySearch(ctx, text, hint); serr == nil {
			return found, nil
		}
		if gerr == nil {
			return pl, nil
		}
		return place{}, gerr
	}
	found, serr := a.locateBySearch(ctx, text, city)
	if serr == nil {
		return found, nil
	}
	pl, _, gerr := a.locateByGeocode(ctx, text, city)
	if gerr == nil {
		return pl, nil
	}
	var nf errNotFound
	if errors.As(serr, &nf) {
		return place{}, gerr
	}
	return place{}, serr
}

// locateByGeocode is the first geocoding match; rough says it is no finer than a district.
func (a *app) locateByGeocode(ctx context.Context, text, city string) (place, bool, error) {
	gs, err := a.geocode(ctx, text, city)
	if err != nil {
		return place{}, false, err
	}
	for _, g := range gs {
		if p, ok := parseAMapLocation(g.Location.String()); ok {
			where := g.City.String()
			if where == "" {
				where = g.Province.String()
			}
			if d := g.District.String(); d != "" && d != where {
				where += d
			}
			return place{name: text, at: p, citycode: g.CityCode.String(), adcode: g.AdCode.String(), where: where}, coarseLevels[g.Level.String()], nil
		}
	}
	return place{}, false, errNotFound{text}
}

// locateBySearch is the first keyword search match, preferring city.
func (a *app) locateBySearch(ctx context.Context, text, city string) (place, error) {
	q := url.Values{"keywords": {text}, "page_size": {"1"}}
	if city != "" {
		q.Set("region", city)
	}
	body, err := a.currentClient().get(ctx, groupSearch, "/v5/place/text", q, geoFor)
	if err != nil {
		return place{}, err
	}
	var ans poiAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return place{}, errBadAnswer
	}
	for _, p := range ans.Pois {
		if at, ok := parseAMapLocation(p.Location.String()); ok {
			where := p.CityName.String()
			if d := p.AdName.String(); d != "" && d != where {
				where += d
			}
			return place{name: p.Name.String(), at: at, citycode: p.CityCode.String(), adcode: p.AdCode.String(), where: where}, nil
		}
	}
	return place{}, errNotFound{text}
}

// cityCode is the city code of point p (reverse geocoding, LBS allowance), for public transport.
func (a *app) cityCode(ctx context.Context, p point) (string, error) {
	r, err := a.reverse(ctx, point{math.Round(p.Lat*1e4) / 1e4, math.Round(p.Lng*1e4) / 1e4}, false)
	if err != nil {
		return "", err
	}
	if c := r.AddressComponent.V.CityCode.String(); c != "" {
		return c, nil
	}
	return "", errors.New("高德不知道这个点在哪个城市，公交路线算不了")
}

// reverse is reverse geocoding of p; all asks for the nearby places too.
func (a *app) reverse(ctx context.Context, p point, all bool) (regeocode, error) {
	q := url.Values{"location": {amapLocation(p)}}
	if all {
		q.Set("extensions", "all")
		q.Set("radius", "500")
	}
	body, err := a.currentClient().get(ctx, groupLBS, "/v3/geocode/regeo", q, geoFor)
	if err != nil {
		return regeocode{}, err
	}
	var ans regeoAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return regeocode{}, errBadAnswer
	}
	return ans.Regeocode.V, nil
}

func (a *app) toolSearch(ctx context.Context, in args) (wire.ConnectorCallResult, error) {
	if in.Keywords == "" {
		return wire.ConnectorCallResult{}, errors.New("要给 keywords：搜什么")
	}
	pois, err := a.search(ctx, in.Keywords, in.City, in.CityOnly, limitOr(in.Limit, 5, 10))
	if err != nil {
		return wire.ConnectorCallResult{}, err
	}
	where := "全国"
	if in.City != "" {
		where = in.City
	}
	if len(pois) == 0 {
		return wire.ConnectorCallResult{Text: fmt.Sprintf("高德在%s没搜到「%s」。", where, in.Keywords)}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "高德在%s搜「%s」（坐标为 WGS-84 纬度,经度）：", where, in.Keywords)
	for i, p := range pois {
		b.WriteString("\n" + poiLine(i+1, p, nil))
	}
	res := wire.ConnectorCallResult{Text: b.String()}
	if mc := buildPOIMapComponent(
		"",
		fmt.Sprintf("搜索「%s」", in.Keywords),
		fmt.Sprintf("高德在%s搜索「%s」的结果分布图", where, in.Keywords),
		pois,
		nil,
		"",
		false,
	); mc != nil {
		res.Views = []wire.View{{
			Title:    clipFit(fmt.Sprintf("高德搜索：%s", in.Keywords), 120),
			Sections: []wire.Section{{Items: []wire.Component{*mc}}},
		}}
	}
	return res, nil
}

func (a *app) toolNearby(ctx context.Context, in args) (wire.ConnectorCallResult, error) {
	center, ok, err := pointArg(in.Latitude, in.Longitude, "中心点")
	if err != nil {
		return wire.ConnectorCallResult{}, err
	}
	name := ""
	centerShortName := ""
	if !ok {
		if in.Place == "" {
			return wire.ConnectorCallResult{}, errors.New("要给中心点：latitude/longitude（用户说「附近」时先用 phone_location_current），或 place（地址、地名或常用地点名如「家」「公司」；未配置时可查 amap_places / list_places）")
		}
		pl, err := a.resolvePoint(ctx, in.Place, in.City)
		if err != nil {
			return wire.ConnectorCallResult{}, err
		}
		center, name, centerShortName = pl.at, pl.label(), pl.name
	} else {
		name = center.String()
		centerShortName = "当前位置"
	}
	radius := limitOr(in.RadiusMeters, 1000, 50000)
	limit := limitOr(in.Limit, 5, 10)
	q := url.Values{"location": {amapLocation(center)}, "radius": {strconv.Itoa(radius)},
		"page_size": {strconv.Itoa(limit)}, "show_fields": {"business"}}
	if in.Keywords != "" {
		q.Set("keywords", in.Keywords)
	}
	byWeight := in.Sort == "weight"
	if byWeight {
		q.Set("sortrule", "weight")
	} else {
		q.Set("sortrule", "distance")
	}
	body, err := a.currentClient().get(ctx, groupSearch, "/v5/place/around", q, searchFor)
	if err != nil {
		return wire.ConnectorCallResult{}, err
	}
	var ans poiAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return wire.ConnectorCallResult{}, errBadAnswer
	}
	pois := ans.Pois
	what := in.Keywords
	if what == "" {
		what = "吃饭、购物和生活服务的地方"
	}
	if len(pois) == 0 {
		return wire.ConnectorCallResult{Text: fmt.Sprintf("高德在 %s 周围 %s 内没搜到%s。", name, distanceWords(float64(radius)), quoted(what))}, nil
	}
	if !byWeight {
		// AMap ignores the distance order for a keyword-only search: order what it gave.
		sort.SliceStable(pois, func(i, j int) bool { return poiDistance(pois[i], center) < poiDistance(pois[j], center) })
	}
	var b strings.Builder
	fmt.Fprintf(&b, "高德在 %s 周围 %s 内找%s（坐标为 WGS-84 纬度,经度）：", name, distanceWords(float64(radius)), quoted(what))
	for i, p := range pois {
		b.WriteString("\n" + poiLine(i+1, p, &center))
	}
	res := wire.ConnectorCallResult{Text: b.String()}
	titleSubject := in.Keywords
	if titleSubject == "" {
		titleSubject = "周边地点"
	}
	if mc := buildPOIMapComponent(
		"",
		fmt.Sprintf("%s附近 · %s", clipFit(centerShortName, 30), titleSubject),
		fmt.Sprintf("高德在%s周围 %s 内找到的 %d 处%s", centerShortName, distanceWords(float64(radius)), len(pois), what),
		pois,
		&center,
		centerShortName,
		false,
	); mc != nil {
		res.Views = []wire.View{{
			Title:    clipFit(fmt.Sprintf("高德周边：%s", titleSubject), 120),
			Sections: []wire.Section{{Items: []wire.Component{*mc}}},
		}}
	}
	return res, nil
}

func buildPOIMapComponent(mapKey, title, alt string, pois []poi, center *point, centerName string, showList bool) *wire.Component {
	pins := make([]placePin, 0, len(pois)+1)
	var firstPt point
	var firstName string
	hasFirst := false

	if center != nil && center.valid() {
		label := clipFit(centerName, 180)
		if label == "" {
			label = "搜索中心"
		}
		pins = append(pins, placePin{
			id:       "center",
			at:       *center,
			kind:     wire.MapMarkerKindPin,
			label:    label,
			subtitle: "搜索中心",
			icon:     markerIconForName(centerName),
			tone:     wire.TonePositive,
		})
	}

	for i, p := range pois {
		at, ok := parseAMapLocation(p.Location.String())
		if !ok || !at.valid() {
			continue
		}
		poiName := p.Name.String()
		if poiName == "" {
			poiName = fmt.Sprintf("地点 %d", i+1)
		}
		if !hasFirst {
			firstPt, firstName, hasFirst = at, poiName, true
		}
		label := fmt.Sprintf("%d. %s", i+1, poiName)
		sub, detail := poiMetadata(p, center)
		pin := placePin{
			id:       fmt.Sprintf("poi-%d", i+1),
			at:       at,
			kind:     wire.MapMarkerKindPin,
			label:    label,
			subtitle: sub,
			detail:   detail,
			icon:     "location",
		}
		if center == nil && i == 0 {
			pin.tone = wire.TonePositive
		} else {
			pin.color = wire.ColorBlue
		}
		pins = append(pins, pin)
	}

	if !hasFirst {
		return nil
	}
	var links []wire.MapLink
	if firstName != "" {
		links = []wire.MapLink{{
			Title: "在高德地图中打开 " + clipFit(firstName, 30),
			URL:   amapMarkerURL(firstPt, firstName),
		}}
	}
	poiCount := len(pins)
	if center != nil && center.valid() {
		poiCount--
	}
	caption := fmt.Sprintf("共 %d 处地点 · 点击查看位置与详情", poiCount)
	return buildPlacesMapComponent(mapKey, title, alt, caption, wire.ImageAspectWide, nil, pins, links, showList)
}

func poiMetadata(p poi, center *point) (subtitle, detail string) {
	var sub []string
	if center != nil {
		if d := poiDistance(p, *center); !math.IsInf(d, 1) {
			sub = append(sub, distanceWords(d))
		}
	}
	if c := category(p.Type.String()); c != "" && !strings.Contains(p.Name.String(), c) {
		sub = append(sub, c)
	}
	addr := address(p.CityName.String(), p.AdName.String(), p.Address.String())
	if addr != "" && (center == nil || len(sub) < 2) {
		sub = append(sub, addr)
	}

	var details []string
	if addr != "" {
		details = append(details, "地址："+addr)
	}
	biz := p.Business.V
	if tel := phones(biz.Tel.String()); tel != "" {
		details = append(details, "电话："+tel)
	}
	if h := biz.OpentimeToday.String(); h != "" {
		details = append(details, "营业时间："+h)
	}
	if r, ok := biz.Rating.num(); ok && r > 0 {
		details = append(details, "评分 "+biz.Rating.String())
	}
	if c, ok := biz.Cost.num(); ok && c > 0 {
		details = append(details, "人均 ¥"+strconv.FormatFloat(c, 'f', -1, 64))
	}
	return clipFit(strings.Join(sub, " · "), 200), clipFit(strings.Join(details, " · "), 2000)
}

func quoted(s string) string {
	if strings.HasSuffix(s, "地方") {
		return s
	}
	return "「" + s + "」"
}

// poiDistance is AMap's distance of p, or the distance from center when AMap gave none.
func poiDistance(p poi, center point) float64 {
	if d, ok := p.Distance.num(); ok {
		return d
	}
	if at, ok := parseAMapLocation(p.Location.String()); ok {
		return metres(center, at)
	}
	return math.Inf(1)
}

// poiLine writes one place: name (category) · distance from center (when given) · address · point · phone ·
// today's hours · rating · cost.
func poiLine(i int, p poi, center *point) string {
	parts := []string{p.Name.String()}
	if c := category(p.Type.String()); c != "" && !strings.Contains(parts[0], c) {
		parts[0] += "（" + c + "）"
	}
	if center != nil {
		if d := poiDistance(p, *center); !math.IsInf(d, 1) {
			parts = append(parts, distanceWords(d))
		}
	}
	if addr := address(p.CityName.String(), p.AdName.String(), p.Address.String()); addr != "" {
		parts = append(parts, addr)
	}
	if at, ok := parseAMapLocation(p.Location.String()); ok {
		parts = append(parts, at.String())
	}
	biz := p.Business.V
	if tel := phones(biz.Tel.String()); tel != "" {
		parts = append(parts, "电话 "+tel)
	}
	if h := biz.OpentimeToday.String(); h != "" {
		parts = append(parts, "今天 "+h)
	}
	if r, ok := biz.Rating.num(); ok && r > 0 {
		parts = append(parts, "评分 "+biz.Rating.String())
	}
	if c, ok := biz.Cost.num(); ok && c > 0 {
		parts = append(parts, "人均 ¥"+strconv.FormatFloat(c, 'f', -1, 64))
	}
	return fmt.Sprintf("%d. %s", i, strings.Join(parts, " · "))
}

// category is the useful part of AMap's "大类;中类;小类" type: the middle one, or the last when the middle only
// repeats the first (风景名胜;风景名胜;国家级景点).
func category(t string) string {
	t, _, _ = strings.Cut(t, "|")
	parts := strings.Split(t, ";")
	switch {
	case len(parts) >= 3 && parts[1] == parts[0]:
		return parts[2]
	case len(parts) >= 2:
		return parts[1]
	}
	return parts[0]
}

// address joins city, district and street address, without repeating what the address already says.
func address(city, district, addr string) string {
	out := ""
	for _, part := range []string{city, district} {
		if part != "" && !strings.Contains(addr, part) && !strings.HasSuffix(out, part) {
			out += part
		}
	}
	return out + addr
}

// phones keeps at most two of AMap's ";"-separated numbers.
func phones(s string) string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p != "" && len(out) < 2 {
			out = append(out, p)
		}
	}
	return strings.Join(out, "、")
}

// distanceWords writes metres as 米 below a kilometre and 公里 with one decimal above.
func distanceWords(m float64) string {
	if m < 1000 {
		return fmt.Sprintf("%.0f 米", m)
	}
	return strconv.FormatFloat(math.Round(m/100)/10, 'f', -1, 64) + " 公里"
}

func (a *app) toolGeocode(ctx context.Context, in args) (string, error) {
	if in.Address == "" {
		return "", errors.New("要给 address：地址")
	}
	gs, err := a.geocode(ctx, in.Address, in.City)
	if err != nil {
		return "", err
	}
	if len(gs) == 0 {
		return fmt.Sprintf("高德没找到「%s」这个地址；店名、景点名用 amap_search 搜。", in.Address), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "「%s」的坐标（WGS-84 纬度,经度）：", in.Address)
	for i, g := range gs {
		if i == 3 {
			break
		}
		at, ok := parseAMapLocation(g.Location.String())
		if !ok {
			continue
		}
		parts := []string{g.FormattedAddress.String(), at.String()}
		if l := g.Level.String(); l != "" {
			parts = append(parts, "精度："+l)
		}
		if c := strings.TrimSpace(g.City.String() + " " + g.District.String()); c != "" {
			parts = append(parts, c)
		}
		if ad := g.AdCode.String(); ad != "" {
			parts = append(parts, "adcode "+ad)
		}
		fmt.Fprintf(&b, "\n%d. %s", i+1, strings.Join(parts, " · "))
	}
	return b.String(), nil
}

func (a *app) toolReverse(ctx context.Context, in args) (string, error) {
	p, ok, err := pointArg(in.Latitude, in.Longitude, "")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("要给 latitude 和 longitude（WGS-84）")
	}
	r, err := a.reverse(ctx, p, true)
	if err != nil {
		return "", err
	}
	addr := r.FormattedAddress.String()
	if addr == "" {
		return fmt.Sprintf("高德不知道 %s 是哪儿（可能在境外或海上）。", p), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s（WGS-84）在：%s", p, addr)
	ac := r.AddressComponent.V
	if sn := ac.StreetNumber.V; sn.Street.String() != "" {
		fmt.Fprintf(&b, "\n门牌：%s%s", sn.Street, sn.Number)
		if d, ok := sn.Distance.num(); ok {
			fmt.Fprintf(&b, "（%s%s）", sn.Direction, distanceWords(d))
		}
	}
	var in2 []string
	for _, aoi := range r.Aois {
		if d, ok := aoi.Distance.num(); ok && d == 0 && len(in2) < 2 {
			in2 = append(in2, aoi.Name.String())
		}
	}
	if len(in2) > 0 {
		b.WriteString("\n在：" + strings.Join(in2, "、") + " 里")
	}
	var areas []string
	for _, ba := range ac.BusinessAreas {
		if n := ba.Name.String(); n != "" && len(areas) < 3 {
			areas = append(areas, n)
		}
	}
	if len(areas) > 0 {
		b.WriteString("\n商圈：" + strings.Join(areas, "、"))
	}
	var near []string
	for _, np := range r.Pois {
		if len(near) == 3 {
			break
		}
		s := np.Name.String()
		var bits []string
		if c := category(np.Type.String()); c != "" {
			bits = append(bits, c)
		}
		if d, ok := np.Distance.num(); ok {
			bits = append(bits, strings.TrimSpace(np.Direction.String()+" "+distanceWords(d)))
		}
		if len(bits) > 0 {
			s += "（" + strings.Join(bits, "，") + "）"
		}
		near = append(near, s)
	}
	if len(near) > 0 {
		b.WriteString("\n附近：" + strings.Join(near, "；"))
	}
	return b.String(), nil
}

func (a *app) toolInputTips(ctx context.Context, in args) (string, error) {
	if in.Keywords == "" {
		return "", errors.New("要给 keywords：要联想或补全的关键词")
	}
	center, hasPoint, err := pointArg(in.Latitude, in.Longitude, "偏好位置")
	if err != nil {
		return "", err
	}
	q := url.Values{"keywords": {in.Keywords}, "datatype": {"all"}}
	if in.City != "" {
		q.Set("city", in.City)
		if in.CityOnly {
			q.Set("citylimit", "true")
		}
	}
	if hasPoint {
		q.Set("location", amapLocation(center))
	}
	body, err := a.currentClient().get(ctx, groupSearch, "/v3/assistant/inputtips", q, searchFor)
	if err != nil {
		return "", err
	}
	var ans tipsAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return "", errBadAnswer
	}
	where := "全国"
	if in.City != "" {
		where = in.City
	}
	if len(ans.Tips) == 0 {
		return fmt.Sprintf("高德在%s没有找到「%s」的输入提示。", where, in.Keywords), nil
	}
	limit := limitOr(in.Limit, 5, 10)
	var b strings.Builder
	fmt.Fprintf(&b, "高德在%s给「%s」的输入提示（坐标为 WGS-84 纬度,经度）：", where, in.Keywords)
	shown := 0
	for _, t := range ans.Tips {
		if shown == limit {
			break
		}
		name := t.Name.String()
		if name == "" {
			continue
		}
		shown++
		parts := []string{name}
		if d := t.District.String(); d != "" {
			parts = append(parts, d)
		}
		if addr := t.Address.String(); addr != "" {
			parts = append(parts, addr)
		}
		if at, ok := parseAMapLocation(t.Location.String()); ok {
			parts = append(parts, at.String())
		}
		if ad := t.AdCode.String(); ad != "" {
			parts = append(parts, "adcode "+ad)
		}
		fmt.Fprintf(&b, "\n%d. %s", shown, strings.Join(parts, " · "))
	}
	if shown == 0 {
		return fmt.Sprintf("高德在%s没有找到「%s」的输入提示。", where, in.Keywords), nil
	}
	return b.String(), nil
}

func (a *app) toolPlaces(_ context.Context, in args) (string, error) {
	if in.Name != "" {
		fp, ok := a.matchFixedPlace(in.Name)
		if !ok {
			all := a.fixedPlaces()
			if len(all) == 0 {
				return fmt.Sprintf("高德地图扩展里还没有配置「%s」或其他常用地点。可以在 设置 › 扩展 › 高德地图 或「高德地图 › 常用地点」页面添加；如果在 Persona 通用常用地点（list_places）里存过，也可以先查 list_places 拿到地址或坐标。", in.Name), nil
			}
			names := make([]string, len(all))
			for i, p := range all {
				names[i] = p.Name
			}
			return fmt.Sprintf("高德地图扩展的常用地点里没有「%s」（目前已配置：%s）。如果不确定通用常用地点，也可以查 list_places。", in.Name, strings.Join(names, "、")), nil
		}
		return formatFixedPlaceAnswer(fp), nil
	}
	all := a.fixedPlaces()
	if len(all) == 0 {
		return "高德地图扩展里还没有配置常用地点。用户可以在 设置 › 扩展 › 高德地图 填写「家」「公司」「其他常用地点」，或在「高德地图 › 常用地点」页面添加；如果在 Persona 通用常用地点里存过，也可以用 list_places 查看。", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "高德地图已配置的常用地点（共 %d 个；坐标均为 WGS-84）：", len(all))
	for i, fp := range all {
		line := fmt.Sprintf("\n%d. %s · %s · 来源：%s", i+1, fp.Name, fp.summary(), fp.sourceLabel())
		if len(fp.Aliases) > 0 {
			line += "（别名：" + strings.Join(fp.Aliases, "、") + "）"
		}
		b.WriteString(line)
	}
	b.WriteString("\n在 amap_route、amap_nearby、amap_distance、amap_static_map、amap_weather 里可直接填名称（如「家」「公司」），不花高德额度。")
	return b.String(), nil
}

func formatFixedPlaceAnswer(fp fixedPlace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "高德地图常用地点「%s」：\n- 位置：%s\n- 来源：%s", fp.Name, fp.summary(), fp.sourceLabel())
	if len(fp.Aliases) > 0 {
		fmt.Fprintf(&b, "\n- 可直接使用的别名：%s", strings.Join(fp.Aliases, "、"))
	}
	fmt.Fprintf(&b, "\n- 用法：在 amap_route / amap_nearby / amap_distance / amap_static_map / amap_weather 里可直接填「%s」", fp.Name)
	if fp.HasCoord {
		fmt.Fprintf(&b, "，也可直接传 WGS-84 坐标 %s。", fp.coord().String())
	} else {
		b.WriteString("（由扩展自动解析位置）。")
	}
	return b.String()
}
