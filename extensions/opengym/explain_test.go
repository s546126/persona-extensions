package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"
)

// emptyOf is the page's first empty state and its section's footer.
func emptyOf(t *testing.T, v *wire.View) (wire.Component, string) {
	t.Helper()
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == wire.ComponentEmpty {
				return c, s.Footer
			}
		}
	}
	t.Fatalf("no empty state in %+v", v)
	return wire.Component{}, ""
}

func TestSetupPageSaysWhatToFillInAndWhere(t *testing.T) {
	var m struct {
		Name          string
		Configuration struct {
			Properties map[string]struct{ Title string }
		}
		Secrets []struct{ Name, Title string }
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != displayName || !strings.Contains(settingsPath, m.Name) {
		t.Fatalf("the settings path %s does not name the extension %q", settingsPath, m.Name)
	}
	ctx := context.Background()
	for _, c := range []struct {
		name, server, token string
		badges              []string
		extra, card         string
	}{
		{"nothing", "", "", []string{"还没填", "还没填"}, "", "填好「openGym 服务器地址」和「访问密钥（API 令牌）」"},
		{"no key", "https://gym.example.com", "", []string{"已填", "还没填"}, "", "填好「访问密钥（API 令牌）」"},
		{"public http", "http://gym.example.com", fakeToken, []string{"用不了", "已填"}, "填的服务器地址用不了", "改好「openGym 服务器地址」"},
	} {
		a := newApp(t.Logf)
		a.configure(config{ServerURL: c.server}, c.token)
		v, _ := a.renderPage(ctx, uiRequest(pageUI, 6, "", "", nil))
		raw := checkView(t, v, false)
		for _, w := range []string{"这一页会显示你在 openGym 里记的训练", settingsPath, "例如 https://gym.example.com",
			m.Configuration.Properties["server_url"].Title, m.Secrets[0].Title, "「设置 › 集成 › API 令牌」", c.extra} {
			if !strings.Contains(raw, w) {
				t.Errorf("%s: page has no %q: %s", c.name, w, raw)
			}
		}
		var badges []string
		for _, s := range v.Sections {
			for _, it := range s.Items {
				if it.Type == wire.ComponentRow && it.Badge != nil {
					badges = append(badges, it.Badge.Text)
				}
			}
		}
		if strings.Join(badges, ",") != strings.Join(c.badges, ",") {
			t.Errorf("%s: badges %v, want %v", c.name, badges, c.badges)
		}
		if strings.Contains(raw, "BASE_PATH") || strings.Contains(raw, "hub") {
			t.Errorf("%s: jargon on the page: %s", c.name, raw)
		}
		card, _ := a.renderCard(ctx, uiRequest(cardUI, 6, "", "", nil))
		if raw := checkView(t, card, true); !strings.Contains(raw, settingsPath) || !strings.Contains(raw, c.card) {
			t.Errorf("%s: card %s", c.name, raw)
		}
	}
	// an older app gets the same words as text
	a := newApp(t.Logf)
	a.configure(config{}, "")
	v, _ := a.renderPage(ctx, uiRequest(pageUI, 1, "", "", nil))
	if raw := checkView(t, v, false); !strings.Contains(raw, settingsPath) {
		t.Fatalf("v1 setup page %s", raw)
	}
}

func TestReadFailuresSayTheCauseAndWhatToDo(t *testing.T) {
	ctx := context.Background()
	closed := httptest.NewServer(http.NotFoundHandler())
	gone := closed.URL
	closed.Close()
	refused := newFakeGym(t)
	refused.with(func() { refused.refuse = true })
	wrong := newFakeGym(t)
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not openGym</html>"))
	}))
	t.Cleanup(html.Close)
	boom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(boom.Close)
	for _, c := range []struct {
		name, url, title, do, raw string
	}{
		{"unreachable", gone, "连不上 openGym 服务器", "运行 Persona 的那台电脑能不能打开这个地址", "connection refused"},
		{"refused", refused.url(), "openGym 不认这个访问密钥", "「设置 › 集成 › API 令牌」里新建一个", "401"},
		{"wrong path", wrong.srv.URL + "/wrong", "这个地址上找不到 openGym", "地址要带上这段路径", "404"},
		{"not openGym", html.URL, "这个地址回答的不像 openGym", "可能填成了别的网站", "JSON"},
		{"server error", boom.URL, "openGym 服务器出错了", "过一会儿再点「重试」", "500"},
	} {
		a := newApp(t.Logf)
		a.now = today
		a.configure(config{ServerURL: c.url}, fakeToken)
		v, _ := a.renderPage(ctx, uiRequest(pageUI, 6, "", "", nil))
		raw := checkView(t, v, false)
		e, footer := emptyOf(t, v)
		if e.Title != c.title || !strings.Contains(e.Detail, c.do) {
			t.Errorf("%s: empty %q / %q", c.name, e.Title, e.Detail)
		}
		if strings.Contains(e.Title+e.Detail, c.raw) || !strings.HasPrefix(footer, "详细信息：") || !strings.Contains(footer, c.raw) {
			t.Errorf("%s: the raw error belongs in the footer only: %q / %q / %q", c.name, e.Title, e.Detail, footer)
		}
		if !strings.Contains(raw, `"refresh"`) || strings.Contains(raw, fakeToken) {
			t.Errorf("%s: page %s", c.name, raw)
		}
		card, _ := a.renderCard(ctx, uiRequest(cardUI, 6, "", "", nil))
		if raw := checkView(t, card, true); !strings.Contains(raw, c.title) || strings.Contains(raw, c.raw) {
			t.Errorf("%s: card %s", c.name, raw)
		}
	}
}

func TestStaleDataSaysWhyAndKeepsTheDetailSecondary(t *testing.T) {
	f := newFakeGym(t)
	a := testApp(t, f, "")
	f.with(func() { f.refuse = true })
	ctx := context.Background()
	res, _ := a.pageAction(ctx, uiRequest(pageUI, 6, "refresh", "", nil))
	if res.Toast != "没连上：openGym 不认这个访问密钥" {
		t.Fatalf("toast %q", res.Toast)
	}
	top := res.View.Sections[0].Items
	if len(top) != 3 || top[0].Tone != wire.ToneCaution || !strings.Contains(top[0].Text, "openGym 不认这个访问密钥") ||
		strings.Contains(top[0].Text, "401") || top[1].Style != wire.TextStyleSecondary ||
		top[2].Style != wire.TextStyleCaption || !strings.Contains(top[2].Text, "401") {
		t.Fatalf("top %+v", top)
	}
	// the status and the tools keep the error's own words
	if st := a.status(); !strings.Contains(st["error"].(string), "401") {
		t.Fatalf("status %+v", st)
	}
}
