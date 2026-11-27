package tgbot

import (
	"embed"
	"encoding/json"
	"strings"
)

//go:embed locales/*.json
var localeFiles embed.FS

var catalogs = func() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, code := range []string{"en", "zh-Hans", "ja"} {
		data, err := localeFiles.ReadFile("locales/" + code + ".json")
		if err != nil {
			panic(err)
		}
		var m map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			panic(err)
		}
		out[code] = m
	}
	return out
}()

func langOf(locale string) string {
	l := strings.ToLower(strings.TrimSpace(locale))
	switch {
	case strings.HasPrefix(l, "zh"):
		return "zh-Hans"
	case strings.HasPrefix(l, "ja"):
		return "ja"
	case l == "":
		return "zh-Hans"
	default:
		return "en"
	}
}

func t(lang, key string, kv ...string) string {
	m := catalogs[lang]
	if m == nil {
		m = catalogs["en"]
	}
	s := m[key]
	if s == "" {
		s = catalogs["en"][key]
	}
	for i := 0; i+1 < len(kv); i += 2 {
		s = strings.ReplaceAll(s, "{"+kv[i]+"}", kv[i+1])
	}
	return s
}
