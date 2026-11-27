package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"
)

// What the page and the card say when nothing can be read: what is missing and where to fill it in, or why the
// server gave nothing and what to do. A page cannot link to the extension's settings, so the way there is said in
// words. The error's own text stays the same for the status and the tools; the page shows it last, as detail.

// settingsPath is the way to the settings form; the name is the manifest's (TestManifestMatchesTheCode).
const (
	displayName  = "openGym 健身记录"
	settingsPath = "「设置 › 扩展 › " + displayName + "」"
	tokenWhere   = "在 openGym App 的「设置 › 集成 › API 令牌」里"
	// The two things to fill in, by the titles the settings form shows (TestSetupPageSaysWhatToFillInAndWhere).
	serverTitle = "openGym 服务器地址"
	tokenTitle  = "访问密钥（API 令牌）"
)

// setupNeeded is the one sentence the status, the tools, the writes and the card say while the address or the key
// is missing or the address cannot be used: what to fill in, and where. The key is not required by the manifest, so
// the program runs before it is filled in and this is the only guidance the user gets.
func setupNeeded(h health) string {
	server, token := "「"+serverTitle+"」", "「"+tokenTitle+"」"
	var steps []string
	why := ""
	switch {
	case errors.Is(h.Config, errNoServer) && !h.HasToken:
		steps = append(steps, "填好"+server+"和"+token)
	case errors.Is(h.Config, errNoServer):
		steps = append(steps, "填好"+server)
	case h.Config != nil && !errors.Is(h.Config, errNoToken):
		why = "填的服务器地址用不了（" + clip(oneLine(h.Config.Error()), 200) + "）。"
		steps = append(steps, "改好"+server)
		if !h.HasToken {
			steps = append(steps, "再填好"+token)
		}
	case !h.HasToken:
		steps = append(steps, "填好"+token)
	}
	if len(steps) == 0 {
		return "还没连上 openGym：到" + settingsPath + "看看服务器地址和访问密钥。"
	}
	return "还没连上 openGym：" + why + "到" + settingsPath + strings.Join(steps, "，") + "。"
}

// problem is a cause in plain words and what to do about it.
type problem struct {
	Title string
	Do    string
}

// explain turns a failed read into a problem.
func explain(err error) problem {
	var ae *apiError
	var re *reachError
	var an *answerError
	switch {
	case errors.As(err, &ae):
		switch {
		case ae.Status == http.StatusUnauthorized:
			return problem{"openGym 不认这个访问密钥", "密钥可能填错了、过期了，或在 openGym 里被删掉了。" + tokenWhere +
				"新建一个，填到" + settingsPath + "的「访问密钥」里。"}
		case ae.Status == http.StatusForbidden:
			return problem{"openGym 不让这个访问密钥读数据", tokenWhere + "看看这个密钥的权限：训练、计划、体重至少要选「查看」。"}
		case ae.Status == http.StatusNotFound:
			return problem{"这个地址上找不到 openGym", "检查" + settingsPath + "里的服务器地址。openGym 装在网址下面某个路径里的话" +
				"（例如 https://example.com/gym），地址要带上这段路径。openGym 服务器没打开 API 令牌功能也会这样。"}
		case ae.Status == http.StatusTooManyRequests:
			return problem{"openGym 说请求太多了", "等一两分钟再点「重试」。"}
		case ae.Status == http.StatusConflict && ae.Code == "not-synced":
			return problem{"这个 openGym 账号还没有数据", "在手机上的 openGym App 里同步一次，再点「重试」。"}
		case ae.Status >= 500:
			return problem{"openGym 服务器出错了", "问题在服务器那边。过一会儿再点「重试」；一直这样的话，看看 openGym 服务器是不是正常运行。"}
		}
		return problem{"openGym 没有正常回答", "过一会儿再点「重试」；一直这样的话，检查" + settingsPath + "里的服务器地址。"}
	case errors.As(err, &re):
		return problem{"连不上 openGym 服务器", "看看 openGym 服务器开着没有，运行 Persona 的那台电脑能不能打开这个地址。地址填错了也会这样，可以在" +
			settingsPath + "里检查。"}
	case errors.As(err, &an):
		return problem{"这个地址回答的不像 openGym", "可能填成了别的网站，或网址少了一段路径。检查" + settingsPath + "里的服务器地址。"}
	}
	return problem{"没能从 openGym 读到数据", "过一会儿再点「重试」。"}
}

// detail is the error's own words, for the secondary line.
func detail(err error) string { return "详细信息：" + clip(oneLine(err.Error()), 300) }

func secondary(s string) wire.Component {
	return wire.Component{Type: wire.ComponentText, Text: s, Style: wire.TextStyleSecondary}
}

func caption(s string) wire.Component {
	return wire.Component{Type: wire.ComponentText, Text: s, Style: wire.TextStyleCaption}
}

// setupSections is the page before the address and the key are filled in: what the extension is for, then the two
// settings to fill in, each marked filled in or not.
func setupSections(version int, h health) []wire.Section {
	intro := empty(version, "还没连上你的 openGym",
		"这一页会显示你在 openGym 里记的训练：本周练了几次、下一次练什么、最近的训练和体重变化。连上以后，助手也能帮你查训练、记训练。")
	server, serverTone := "已填", wire.TonePositive
	var bad []wire.Component
	switch {
	case errors.Is(h.Config, errNoServer):
		server, serverTone = "还没填", wire.ToneCaution
	case h.Config != nil && !errors.Is(h.Config, errNoToken):
		server, serverTone = "用不了", wire.ToneDanger
		bad = append(bad, caution("填的服务器地址用不了："+h.Config.Error()))
	}
	token, tokenTone := "还没填", wire.ToneCaution
	if h.HasToken {
		token, tokenTone = "已填", wire.TonePositive
	}
	how := wire.Section{Title: "怎么连上", Footer: "填好保存后，回到这一页就能看到你的训练。", Items: append([]wire.Component{
		text("打开" + settingsPath + "，填好这两项："),
		{Type: wire.ComponentRow, Title: serverTitle, Icon: wire.NavIconLink,
			Subtitle: "你自己搭的 openGym 的网址，例如 https://gym.example.com；装在网址下面某个路径里的话把路径也带上，例如 https://example.com/gym；家里网络里的也可以是 http://192.168.1.20:8080",
			Badge:    &wire.Badge{Text: server, Tone: serverTone}},
		{Type: wire.ComponentRow, Title: tokenTitle, Icon: wire.NavIconLock,
			Subtitle: tokenWhere + "新建一个，复制过来填上。它以 ogt_ 开头，只显示一次。只想看数据：训练、计划、体重选「查看」；想让助手帮你记训练、建计划：选「应用」。",
			Badge:    &wire.Badge{Text: token, Tone: tokenTone}},
	}, bad...)}
	return []wire.Section{{Items: []wire.Component{intro}}, how}
}
