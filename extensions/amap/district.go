package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Administrative districts (/v3/config/district, the LBS allowance): province / city / district / street hierarchy,
// adcodes, citycodes and WGS-84 centers.

const districtChildrenMost = 40

var districtLevelWords = map[string]string{
	"country":  "国家",
	"province": "省/直辖市",
	"city":     "市",
	"district": "区县",
	"street":   "街道/乡镇",
}

func districtLevel(level string) string {
	if w := districtLevelWords[level]; w != "" {
		return w
	}
	return level
}

func (a *app) toolDistrict(ctx context.Context, in args) (string, error) {
	sub := 1
	if in.Subdistrict != nil {
		sub = max(0, min(*in.Subdistrict, 2))
	}
	q := url.Values{
		"subdistrict": {strconv.Itoa(sub)},
		"extensions":  {"base"},
	}
	if in.Keywords != "" {
		q.Set("keywords", in.Keywords)
	}
	body, err := a.currentClient().get(ctx, groupLBS, "/v3/config/district", q, geoFor)
	if err != nil {
		return "", err
	}
	var ans districtAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return "", errBadAnswer
	}
	if len(ans.Districts) == 0 {
		if in.Keywords == "" {
			return "高德没有返回行政区划信息。", nil
		}
		return fmt.Sprintf("高德没找到行政区「%s」。", in.Keywords), nil
	}
	var b strings.Builder
	title := "中国"
	if in.Keywords != "" {
		title = "「" + in.Keywords + "」"
	}
	fmt.Fprintf(&b, "高德行政区划查询 %s（中心坐标为 WGS-84 纬度,经度）：", title)
	limit := min(len(ans.Districts), 5)
	for i := range limit {
		writeDistrict(&b, ans.Districts[i], 0)
	}
	if len(ans.Districts) > limit {
		fmt.Fprintf(&b, "\n（还有 %d 个同名行政区，请加上省市名缩小范围）", len(ans.Districts)-limit)
	}
	return b.String(), nil
}

func writeDistrict(b *strings.Builder, d district, depth int) {
	indent := strings.Repeat("  ", depth)
	prefix := "- "
	if depth == 0 {
		prefix = ""
	}
	parts := []string{d.Name.String()}
	if l := districtLevel(d.Level.String()); l != "" {
		parts[0] += "（" + l + "）"
	}
	if ad := d.AdCode.String(); ad != "" {
		parts = append(parts, "adcode "+ad)
	}
	if cc := d.CityCode.String(); cc != "" {
		parts = append(parts, "区号 "+cc)
	}
	if at, ok := parseAMapLocation(d.Center.String()); ok {
		parts = append(parts, "中心 "+at.String())
	}
	fmt.Fprintf(b, "\n%s%s%s", indent, prefix, strings.Join(parts, " · "))

	if len(d.Districts) == 0 {
		return
	}
	shown := min(len(d.Districts), districtChildrenMost)
	if depth >= 1 {
		// Compact one-line summary for deeper levels to keep the answer concise.
		var names []string
		for i := range shown {
			c := d.Districts[i]
			if ad := c.AdCode.String(); ad != "" {
				names = append(names, fmt.Sprintf("%s(%s)", c.Name.String(), ad))
			} else {
				names = append(names, c.Name.String())
			}
		}
		if len(d.Districts) > shown {
			names = append(names, fmt.Sprintf("…等共 %d 个", len(d.Districts)))
		}
		fmt.Fprintf(b, "\n%s  下辖：%s", indent, strings.Join(names, "、"))
		return
	}
	for i := range shown {
		writeDistrict(b, d.Districts[i], depth+1)
	}
	if len(d.Districts) > shown {
		fmt.Fprintf(b, "\n%s  （下辖共 %d 个，已显示前 %d 个）", indent, len(d.Districts), shown)
	}
}
