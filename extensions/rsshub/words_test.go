package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// jargon is what a first-time user should not have to know. RSS and RSSHub are named, and explained on the page; a
// failure's raw message may still say any of these, in small print after detailPrefix.
var jargon = []string{"路由", "实例", "条目", "订阅源", "ETag", "If-Modified-Since", "JSON Feed", "Atom", "JSON", "沙盒",
	"host.persona.internal", "format=", "?key", "?code", "md5", "HTTP", "http(s)", "连通", "404", "403", "429", "dial tcp"}

// fixtureWords are the synthetic feeds' own titles, which are not the page's words.
var fixtureWords = []string{"Example Atom", "Example JSON"}

// loopbackAddrRE matches synthetic httptest loopback host:port values whose ephemeral ports may contain substrings
// such as "404", "403", or "429".
var loopbackAddrRE = regexp.MustCompile(`(?:127\.0\.0\.1|\[::1\]):\d+`)

func findJargon(s string) []string {
	for _, w := range fixtureWords {
		s = strings.ReplaceAll(s, w, "")
	}
	s = loopbackAddrRE.ReplaceAllString(s, "")
	var hits []string
	for _, w := range jargon {
		if strings.Contains(s, w) {
			hits = append(hits, w)
		}
	}
	return hits
}

func speaksPlainly(t *testing.T, where, s string) {
	t.Helper()
	for _, w := range findJargon(s) {
		t.Errorf("%s says %q: %s", where, w, s)
	}
}

func TestFindJargonIgnoresLoopbackPorts(t *testing.T) {
	for _, s := range []string{
		"RSSHub 服务 连不上 127.0.0.1:40485",
		"RSSHub 服务 连不上 127.0.0.1:54032",
		"RSSHub 服务 连不上 127.0.0.1:34291",
		"RSSHub 服务 连不上 [::1]:40404",
	} {
		if hits := findJargon(s); len(hits) != 0 {
			t.Errorf("findJargon(%q) = %v, want none", s, hits)
		}
	}
	for _, tc := range []struct {
		s, want string
	}{
		{"没有这条内容（404）", "404"},
		{"被拒绝 403", "403"},
		{"限流 429", "429"},
		{"连不上 127.0.0.1:40485（404）", "404"},
	} {
		if hits := findJargon(tc.s); !slices.Equal(hits, []string{tc.want}) {
			t.Errorf("findJargon(%q) = %v, want [%q]", tc.s, hits, tc.want)
		}
	}
}

// TestPagesSpeakPlainChinese fails when the page's own words in any state go back to RSSHub or HTTP jargon.
func TestPagesSpeakPlainChinese(t *testing.T) {
	states := pageStates(t)
	if len(states) < 15 {
		t.Fatalf("%d states", len(states))
	}
	for name, sh := range states {
		toast, _, _ := strings.Cut(sh.toast, "（"+detailPrefix)
		speaksPlainly(t, name+" toast", toast)
		speaksPlainly(t, name+" title", sh.view.Title)
		for _, sec := range sh.view.Sections {
			speaksPlainly(t, name+" section", sec.Title+" "+sec.Footer)
			for _, c := range sec.Items {
				if strings.HasPrefix(c.Text, detailPrefix) {
					continue
				}
				words := []string{c.Title, c.Text, c.Label, c.Subtitle, c.Detail, c.Placeholder}
				for _, st := range c.Stats {
					words = append(words, st.Label, st.Value, st.Detail)
				}
				for _, ch := range c.Choices {
					words = append(words, ch.Title)
				}
				if c.Badge != nil {
					words = append(words, c.Badge.Text)
				}
				if c.Button != nil {
					words = append(words, c.Button.Title)
				}
				if c.Action != nil {
					words = append(words, c.Action.Confirm)
				}
				speaksPlainly(t, name+" "+c.Type, strings.Join(words, " "))
			}
		}
	}
	empty := states["01-empty-v4"].view
	raw, _ := json.Marshal(empty)
	for _, want := range []string{introText, "RSS 是", "RSSHub 是", "还没有订阅任何网站", "preview:" + exampleRoute, "先看看", routesDocs} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the empty page does not say %q", want)
		}
	}
	// Before anything is subscribed the service has not been asked yet; the page says nothing needs doing.
	for _, name := range []string{"01-empty-v4", "01-empty-v1"} {
		raw, _ := json.Marshal(states[name].view)
		if !strings.Contains(string(raw), "还没用到") || !strings.Contains(string(raw), serviceIdleNote) || strings.Contains(string(raw), "还没试过") {
			t.Errorf("%s does not say the RSSHub service needs nothing yet: %s", name, raw)
		}
	}
	for _, name := range []string{"05-several-sources", "12-instance-address-wrong", "15-instance-unreachable-ping"} {
		if raw, _ := json.Marshal(states[name].view); strings.Contains(string(raw), serviceIdleNote) {
			t.Errorf("%s says nothing needs doing", name)
		}
	}
	old, _ := json.Marshal(states["01-empty-v1"].view)
	if !strings.Contains(string(old), `"type":"button","title":"试一个例子`) {
		t.Errorf("an older app gets no example button: %s", old)
	}
	for name, want := range map[string][]string{
		"05-several-sources":        {"订阅的网站", `"value":"3"`, "上次检查", "11-20 09:30"},
		"09-sources-failing":        {"有 3 个网站上次没读到", "被对方拦下了", "找不到这个来源", "等了太久没有回应", "出错"},
		"10-failing-feed":           {"找不到这个来源", "怎么办：", detailPrefix + "没有这条路由或订阅源（404）"},
		"12-instance-address-wrong": {"设置里的「RSSHub 服务地址」填得不对", "怎么办："},
	} {
		raw, _ := json.Marshal(states[name].view)
		for _, w := range want {
			if !strings.Contains(string(raw), w) {
				t.Errorf("%s does not say %q", name, w)
			}
		}
	}
}

// TestSettingsSpeakPlainChinese fails when the manifest's words go back to jargon, a setting that rarely needs
// changing stops saying so, or a setting's key or value changes with its words.
func TestSettingsSpeakPlainChinese(t *testing.T) {
	var m struct {
		Name, Description string
		Contributes       struct {
			Connectors []struct{ Name string }
			UI         []struct{ Title string }
		}
		Configuration struct {
			Properties map[string]struct {
				Title, Description string
				OneOf              []struct{ Const, Title string }
			}
		}
		Secrets []struct{ Name, Title, Description string }
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	speaksPlainly(t, "name", m.Name)
	speaksPlainly(t, "description", m.Description)
	for _, c := range m.Contributes.Connectors {
		speaksPlainly(t, "connector", c.Name)
	}
	for _, u := range m.Contributes.UI {
		speaksPlainly(t, "page", u.Title)
		if u.Title != pageTitle {
			t.Errorf("page title %q, the page says %q", u.Title, pageTitle)
		}
	}
	var keys []string
	for k, p := range m.Configuration.Properties {
		keys = append(keys, k)
		speaksPlainly(t, k, p.Title)
		desc := p.Description
		if k == "prefer_json" {
			desc = strings.Replace(desc, "（JSON）", "", 1) // named once, in brackets, as what the switch asks for
		}
		speaksPlainly(t, k, desc)
		for _, o := range p.OneOf {
			speaksPlainly(t, k+" choice", o.Title)
		}
		if k != "refresh_minutes" && !strings.HasPrefix(p.Description, "一般不用改") {
			t.Errorf("%s does not start with 一般不用改: %s", k, p.Description)
		}
	}
	// The form cannot fold settings away, so the two that almost nobody needs say so first thing in their titles.
	for k, p := range m.Configuration.Properties {
		rare := k == "access_auth" || k == "prefer_json"
		if strings.HasPrefix(p.Title, "高级：") != rare {
			t.Errorf("%s title %q: 「高级：」 belongs at the start of access_auth and prefer_json only", k, p.Title)
		}
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"access_auth", "instance_url", "prefer_json", "refresh_minutes"}) {
		t.Fatalf("setting keys %v", keys)
	}
	var values []string
	for _, o := range m.Configuration.Properties["access_auth"].OneOf {
		values = append(values, o.Const)
	}
	if !slices.Equal(values, []string{"key", "code"}) {
		t.Fatalf("access_auth values %v", values)
	}
	for _, s := range m.Secrets {
		speaksPlainly(t, "secret "+s.Name, s.Title+" "+strings.Replace(s.Description, "（ACCESS_KEY）", "", 1))
	}
}

// TestExplainSaysWhatToDo turns each failure the program reports into a cause and a next step, keeping the raw
// message as the detail.
func TestExplainSaysWhatToDo(t *testing.T) {
	base, _ := url.Parse(defaultInstance)
	_, badInput := parseSource("hello world", base)
	local := feedAddressOK(&url.URL{Scheme: "http", Host: "127.0.0.1:9"})
	cases := map[string]error{
		"找不到这个来源":        &httpError{Status: 404},
		"被对方拦下了，没让读":     &httpError{Status: 403, Detail: "Access denied."},
		"检查得太勤，被对方暂时限制了": &httpError{Status: 429},
		"对方暂时出了问题":       &httpError{Status: 503},
		"对方回了一个错误":       &httpError{Status: 500},
		"等了太久没有回应":       netError(context.DeadlineExceeded),
		"连不上对方":          netError(errors.New("dial tcp 192.0.2.1:443: connect: connection refused")),
		"这个地址打开的是普通网页，不是可以订阅的更新列表": errNotFeed,
		"设置里的「RSSHub 服务地址」填得不对":    errAddress,
		"这不像是一个能订阅的地址":             badInput,
		"这个地址是本机或内网的，出于安全不能直接读":    local,
	}
	for cause, err := range cases {
		if err == nil {
			t.Fatalf("no error for %s", cause)
		}
		p := explain(err.Error())
		if p.Cause != cause || p.Todo == "" || p.Detail != err.Error() {
			t.Errorf("%v: %+v, want cause %s", err, p, cause)
		}
		speaksPlainly(t, cause, p.sentence())
	}
	if p := explain("没有订阅：/a 读不到（" + (&httpError{Status: 404}).Error() + "）"); p.Cause != "找不到这个来源" {
		t.Errorf("a wrapped failure: %+v", p)
	}
}
