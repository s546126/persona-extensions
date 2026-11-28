package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Weather (/v3/weather/weatherInfo, the weather allowance): the live report and AMap's forecast of today and the next
// three days, for a Chinese city or district by its adcode. A name is turned into its adcode by geocoding (the LBS
// allowance, cached for a month), and a WGS-84 point or fixed place uses reverse geocoding or place lookup, so a
// question costs one weather request, or two with a forecast.

var adcodeRe = regexp.MustCompile(`^\d{6}$`)

var weekdays = map[string]string{"1": "周一", "2": "周二", "3": "周三", "4": "周四", "5": "周五", "6": "周六", "7": "周日"}

func (a *app) adcodeForPoint(ctx context.Context, p point) (string, error) {
	r, err := a.reverse(ctx, point{math.Round(p.Lat*1e4) / 1e4, math.Round(p.Lng*1e4) / 1e4}, false)
	if err != nil {
		return "", err
	}
	if ad := r.AddressComponent.V.AdCode.String(); adcodeRe.MatchString(ad) {
		return ad, nil
	}
	return "", errors.New("高德不知道这个位置在哪个区县，查不了天气")
}

// adcode finds a city's adcode: as given, by fixed-place lookup, by WGS-84 coordinates, or by geocoding its name.
// fixedLabel is non-empty when city matched a configured/saved fixed place name.
func (a *app) adcode(ctx context.Context, city string) (ad string, fixedLabel string, err error) {
	city = strings.TrimSpace(city)
	if adcodeRe.MatchString(city) {
		return city, "", nil
	}
	if p, ok := parseLatLng(city); ok {
		ad, err := a.adcodeForPoint(ctx, p)
		return ad, "", err
	}
	if fp, ok := a.matchFixedPlace(city); ok {
		if fp.HasCoord {
			ad, err := a.adcodeForPoint(ctx, fp.coord())
			if err != nil {
				return "", "", fmt.Errorf("常用地点「%s」查区划失败：%w", fp.Name, err)
			}
			return ad, fp.Name, nil
		}
		target := fp.Target
		if target == "" {
			target = fp.Name
		}
		if adcodeRe.MatchString(target) {
			return target, fp.Name, nil
		}
		pl, err := a.locate(ctx, target, "")
		if err != nil {
			return "", "", fmt.Errorf("常用地点「%s」（%s）解析失败：%w", fp.Name, target, err)
		}
		if adcodeRe.MatchString(pl.adcode) {
			return pl.adcode, fp.Name, nil
		}
		ad, err := a.adcodeForPoint(ctx, pl.at)
		if err != nil {
			return "", "", fmt.Errorf("常用地点「%s」查区划失败：%w", fp.Name, err)
		}
		return ad, fp.Name, nil
	}
	gs, err := a.geocode(ctx, city, "")
	if err != nil {
		return "", "", err
	}
	for _, g := range gs {
		if ad := g.AdCode.String(); adcodeRe.MatchString(ad) {
			return ad, "", nil
		}
	}
	return "", "", errNotFound{city}
}

func (a *app) weather(ctx context.Context, adcode string, forecastToo bool) (weatherAnswer, error) {
	ext, ttl := "base", liveFor
	if forecastToo {
		ext, ttl = "all", castFor
	}
	body, err := a.currentClient().get(ctx, groupWeather, "/v3/weather/weatherInfo", url.Values{"city": {adcode}, "extensions": {ext}}, ttl)
	if err != nil {
		return weatherAnswer{}, err
	}
	var ans weatherAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return weatherAnswer{}, errBadAnswer
	}
	return ans, nil
}

func (a *app) toolWeather(ctx context.Context, in args) (string, error) {
	if in.City == "" {
		return "", errors.New("要给 city：城市、区县名、6 位 adcode、WGS-84 坐标，或常用地点名（如「家」「公司」）。用户没说哪儿时，问用户，或用 amap_places / list_places 看「家」在哪里")
	}
	days := 3
	if in.Days != nil {
		days = max(0, min(*in.Days, 4))
	}
	ad, fixedLabel, err := a.adcode(ctx, in.City)
	if err != nil {
		return "", err
	}
	now, err := a.weather(ctx, ad, false)
	if err != nil {
		return "", err
	}
	var casts weatherAnswer
	if days > 0 {
		if casts, err = a.weather(ctx, ad, true); err != nil {
			return "", err
		}
	}
	var b strings.Builder
	name := in.City
	if len(now.Lives) > 0 {
		name = placeName(now.Lives[0].Province.String(), now.Lives[0].City.String())
	} else if len(casts.Forecasts) > 0 {
		name = placeName(casts.Forecasts[0].Province.String(), casts.Forecasts[0].City.String())
	}
	if fixedLabel != "" {
		name = fixedLabel + "（" + name + "）"
	}
	b.WriteString(name + " 天气（中国气象局，经高德）：")
	if len(now.Lives) > 0 {
		l := now.Lives[0]
		parts := []string{l.Weather.String()}
		if t := l.Temperature.String(); t != "" {
			parts = append(parts, t+"°C")
		}
		if h := l.Humidity.String(); h != "" {
			parts = append(parts, "湿度 "+h+"%")
		}
		if w := wind(l.WindDirection.String(), l.WindPower.String()); w != "" {
			parts = append(parts, w)
		}
		fmt.Fprintf(&b, "\n实况（%s 发布）：%s", reportTime(l.ReportTime.String()), strings.Join(parts, "，"))
	}
	if days > 0 && len(casts.Forecasts) > 0 {
		cs := casts.Forecasts[0].Casts
		if len(cs) > days {
			cs = cs[:days]
		}
		for _, c := range cs {
			when := c.Date.String()
			if t, err := time.Parse("2006-01-02", when); err == nil {
				when = t.Format("01-02")
			}
			sky := "白天" + c.DayWeather.String() + "，夜间" + c.NightWeather.String()
			if c.DayWeather == c.NightWeather {
				sky = c.DayWeather.String()
			}
			w := wind(c.DayWind.String(), c.DayPower.String())
			if c.NightWind != c.DayWind || c.NightPower != c.DayPower {
				w = "白天" + w + "，夜间" + wind(c.NightWind.String(), c.NightPower.String())
			}
			fmt.Fprintf(&b, "\n- %s %s：%s，%s~%s°C，%s", when, weekdays[c.Week.String()], sky, c.NightTemp, c.DayTemp, w)
		}
	}
	return b.String(), nil
}

// placeName is province and city without repeating the province (北京 东城区 → 北京东城区, 北京 北京市 → 北京市).
func placeName(province, city string) string {
	switch {
	case city == "":
		return province
	case province == "" || strings.HasPrefix(city, province) || strings.HasPrefix(province, strings.TrimSuffix(city, "市")):
		return city
	}
	return province + city
}

// wind writes a direction and AMap's force (in 级) as 东北风 ≤3 级.
func wind(dir, power string) string {
	switch dir {
	case "":
	case "无风向", "旋转不定":
		dir = "风向不定"
	default:
		if !strings.HasSuffix(dir, "风") {
			dir += "风"
		}
	}
	if power != "" {
		power += " 级"
	}
	return strings.TrimSpace(dir + " " + power)
}

// reportTime writes "2026-11-15 18:39:39" as 11-15 18:39.
func reportTime(s string) string {
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.Format("01-02 15:04")
	}
	return s
}
