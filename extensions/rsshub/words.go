package main

import (
	"strings"
)

// The page's words for people who have never heard of RSS or RSSHub: what this is, the two kinds of address one can
// paste, and every failure as what happened and what to do, with the program's own message kept as a detail.

const (
	// exampleRoute is the example the empty page offers to try: a route the public instance serves to anyone.
	exampleRoute      = "/github/trending/daily/go/en"
	exampleRouteTitle = "GitHub 每天最热门的 Go 项目"
	exampleUPRoute    = "/bilibili/user/video/2267573"
	routesDocs        = "https://docs.rsshub.app/routes"

	// serviceIdleNote follows the RSSHub service's address while nothing is subscribed and nothing has been read
	// through it: the service is first asked when a source is previewed or read, or on the page's test button.
	serviceIdleNote = "加订阅时才会去连它，现在什么都不用做"

	introText = "把常看的网站放在一处看更新：订阅之后，这里会定时去看它们有没有新文章，有就列在这里。" +
		"比如订阅一个博客、一个新闻网站，或者某位 B 站 UP 主的新视频。"
	addressHelp = "可以填两种地址：\n" +
		"· 网站自己的 RSS 地址。RSS 是很多博客、新闻网站都提供的「更新列表」，网站上常有一个 RSS 或「订阅」图标，" +
		"复制它的链接就行，常以 /feed、/rss 或 .xml 结尾。\n" +
		"· RSSHub 短地址，以 / 开头。RSSHub 是一个免费服务，能把 B 站、微博、GitHub 这类本身没有 RSS 的网站变成可以订阅的。" +
		"比如 " + exampleUPRoute + " 是一位 B 站 UP 主的新视频，" + exampleRoute + " 是 " + exampleRouteTitle + "。"
	detailPrefix = "详细信息："
)

// problem is a failure in words: what happened and what to do about it, with the raw message for whoever asks.
type problem struct {
	Cause  string // 找不到这个来源
	Todo   string // 核对一下地址……
	Detail string // the program's own message
}

// sentence is the problem as one line: the cause, then what to do.
func (p problem) sentence() string {
	return p.Cause + "。" + p.Todo
}

// toast is the problem for a short message: the cause and what to do first, the raw message last.
func (p problem) toast(prefix string) string {
	s := prefix + p.sentence()
	if p.Detail != "" && p.Detail != p.Cause {
		s += "（" + detailPrefix + clip(oneLine(p.Detail), 120) + "）"
	}
	return s
}

// explain says what a failure message of this program means for the user. The messages come from fetch.go,
// instance.go, feed.go and main.go; the ones that already say it plainly are kept as they are.
func explain(raw string) problem {
	raw = oneLine(raw)
	p := problem{Detail: raw}
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(raw, w) {
				return true
			}
		}
		return false
	}
	switch {
	case raw == errAddress.Error():
		p.Cause = "设置里的「RSSHub 服务地址」填得不对"
		p.Todo = "到这个扩展的设置里改成以 https:// 开头的地址；清空它就用默认的免费公共服务"
	case has("拒绝访问"):
		p.Cause = "被对方拦下了，没让读"
		p.Todo = "免费的公共 RSSHub（rsshub.app）对一部分网站和网络有限制，可以过一会儿再试；常用的话，换成自己搭的 RSSHub 更稳。" +
			"用的是自己搭的，就检查设置里的访问密钥"
	case has("（404）"):
		p.Cause = "找不到这个来源"
		p.Todo = "地址可能写错了，或者这个网站已经不提供了。核对一下地址，或者换一个"
	case has("（429）", "太频繁"):
		p.Cause = "检查得太勤，被对方暂时限制了"
		p.Todo = "过一会儿再试；也可以在设置里把自动检查的间隔调长一点"
	case has("暂时读不到"):
		p.Cause = "对方暂时出了问题"
		p.Todo = "一般过一会儿会自己好，不用处理"
	case has("连接超时"):
		p.Cause = "等了太久没有回应"
		p.Todo = "可能是网络慢或者对方太忙，过一会儿再试；一直这样就检查一下网络"
	case has("请求被取消"):
		p.Cause = "检查被中途打断了"
		p.Todo = "再试一次就好"
	case has(errNotFeed.Error()):
		p.Cause = "这个地址打开的是普通网页，不是可以订阅的更新列表"
		p.Todo = "在网站上找 RSS 或「订阅」图标，复制它的链接；网站没有的话，试试 RSSHub 短地址"
	case has("太大"):
		p.Cause = "这个来源一次给的内容太多，读不下"
		p.Todo = "换一个来源，或者找这个网站更小的分类订阅"
	case has("编码"):
		p.Cause = "这个来源的文字编码认不出来"
		p.Todo = "换一个来源试试"
	case has("跳转"):
		p.Cause = "这个地址把我们转到了别处，出于安全没有跟过去"
		p.Todo = "在浏览器里打开它，复制最后停下来的那个地址再填"
	case has("本机的订阅源", "不能读这个地址", "不能连接"):
		p.Cause = "这个地址是本机或内网的，出于安全不能直接读"
		p.Todo = "装在本机的 RSSHub 请填到设置里的「RSSHub 服务地址」；其他地址请换成公网能打开的"
	case has("连不上"):
		p.Cause = "连不上对方"
		p.Todo = "检查网络是否正常、地址有没有写错；过一会儿再试"
	case has("不是路由也不是网址", "不是网址", "只能是 http(s)", "订阅源只能是", "要写一条", "路由"):
		p.Cause = "这不像是一个能订阅的地址"
		p.Todo = "请粘贴以 https:// 开头的 RSS 地址，或者以 / 开头的 RSSHub 短地址，比如 " + exampleUPRoute
	case has("服务器回答"):
		p.Cause = "对方回了一个错误"
		p.Todo = "过一会儿再试；一直这样就核对一下地址"
	case has("最多订阅"):
		p.Cause = "订阅的数量到上限了"
		p.Todo = "先取消一些不看的，再订阅新的"
	default:
		p.Cause = "没读到"
		p.Todo = "过一会儿再试"
	}
	return p
}
