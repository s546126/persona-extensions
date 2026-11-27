package ui

import (
	"errors"
	"strings"

	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// The words of the pages. The kernel, which the household extension and the assistant's tools share, names its
// concepts in schema terms (类型, 属性, 关联, 阶段 ...); the pages use everyday words instead, one per concept:
//
//	type -> 分类, property -> 填写项, entity -> 条目, relation -> 连到 (别的条目), structure -> 分类设置,
//	stage / lifecycle -> 状态, phase -> 进展 (算作 in the editor), view -> 显示方式, card -> 列表里的样子,
//	rollup -> 统计, derive -> 由它新建, key -> 代号.
//
// Only what the pages show changes here: stored names, keys, action ids and the tools' words stay as they are.

// builtinNames name the built-in fields of every type on the pages. The kernel's own names stay in the base and in
// the tools' answers.
var builtinNames = map[string]string{
	"title": "标题", "status": "状态", "phase": "进展", "type": "分类",
	"origin": "来自", "created": "创建时间", "updated": "更新时间",
}

// kindWord is how the pages name a property kind: its label, the line under it in the list of kinds, and the
// placeholder of its input on an entry.
type kindWord struct{ label, about, placeholder string }

// kindWords are the kinds in everyday words, keyed by the kernel's kind name. A kind missing here keeps the
// kernel's label and hint.
var kindWords = map[string]kindWord{
	kernel.KindText:     {"文字", "一句话或一段话，例如 作者、备注", ""},
	kernel.KindNumber:   {"数字", "例如 页数、价格，可以带单位", ""},
	kernel.KindBoolean:  {"是/否", "打勾或不打勾，例如 已读", ""},
	kernel.KindDate:     {"日期", "例如 读完的日子、下次续费的日子", "例如 2026-12-01"},
	kernel.KindEnum:     {"选项", "从几个固定选项里选一个，例如 评分", ""},
	kernel.KindURL:      {"网址", "例如 文章地址、订阅的管理页", "例如 https://example.com"},
	kernel.KindRelation: {"连到别的条目", "把这条和另一条连起来，例如 笔记出自哪本书", ""},
	kernel.KindRollup:   {"自动统计", "自动数一数连着它的条目，或把它们的数字加起来", ""},
}

// kindLabel is the everyday name of a kind.
func kindLabel(k *kernel.Kind) string {
	if w, ok := kindWords[k.Name]; ok {
		return w.label
	}
	return k.Label
}

// kindAbout is the line under a kind in the list of kinds to add.
func kindAbout(k *kernel.Kind) string {
	if w, ok := kindWords[k.Name]; ok {
		return w.about
	}
	return k.Hint
}

// kindPlaceholder is the placeholder of an entry's input of the kind.
func kindPlaceholder(k *kernel.Kind) string {
	if w, ok := kindWords[k.Name]; ok {
		return w.placeholder
	}
	return k.Hint
}

// fieldOf is the field of the type with the key, as the kernel answers it, with a built-in field named in the
// pages' words.
func (c *ctx) fieldOf(t *kernel.Type, key string) (kernel.Prop, bool) {
	p, ok := c.sp.Field(t, key)
	if name, builtin := builtinNames[key]; ok && builtin {
		p.Name = name
	}
	return p, ok
}

// errWords rewrite the kernel's words in a refused change, in order (longer words first). Quoted names, between
// 「 and 」, are the user's own and are never rewritten.
var errWords = strings.NewReplacer(
	"不是关联", "不是「连到别的条目」", "必须是指向", "必须是连到", "只能关联一条记录", "只能连到一条",
	"最多关联", "最多连到", "指向的类型", "连到的分类", "类型标识", "分类代号", "属性标识", "填写项代号", "属性类型", "填写项种类", "空间编号", "空间代号",
	"新的流程里", "改完以后", "的流程", "的状态", "的主线", "的主要状态",
	"类型", "分类", "属性", "填写项", "阶段", "状态", "关联", "连到", "汇总", "统计", "字段", "填写项",
	"视图", "显示方式", "记录", "条目", "标识", "代号", "不合法", "不对",
)

// plainText rewrites the kernel's words outside the quoted names of a message.
func plainText(msg string) string {
	var b strings.Builder
	for msg != "" {
		open := strings.Index(msg, "「")
		if open < 0 {
			b.WriteString(errWords.Replace(msg))
			break
		}
		b.WriteString(errWords.Replace(msg[:open]))
		rest := msg[open:]
		end := strings.Index(rest, "」")
		if end < 0 {
			b.WriteString(rest)
			break
		}
		end += len("」")
		b.WriteString(rest[:end])
		msg = rest[end:]
	}
	return b.String()
}

// keyHint is what to do about a code the kernel refused.
const keyHint = "代号要用小写英文字母开头，后面只能有小写字母、数字或下划线，例如 film"

// plainError is the toast of a refused change: the cause in the pages' words and, when the cause is a known one,
// what to do about it.
func plainError(err error) string {
	msg := plainText(err.Error())
	switch {
	case strings.Contains(msg, "代号「") && strings.HasSuffix(msg, "不对"):
		msg += "。" + keyHint
	case strings.Contains(msg, "代号「") && strings.HasSuffix(msg, "重复"):
		msg += "，请换一个代号"
	case strings.Contains(msg, "的名称应为"):
		msg += "，请填一个名称"
	case strings.Contains(msg, "」应为"):
		msg += "，请改好再保存"
	case strings.Contains(msg, "不能直接变为"):
		msg = strings.Replace(msg, "；可以变为", "。一次只能往前或往后走一步，现在可以改成", 1)
	}
	return msg
}

// hinted is err with a sentence on what to do after it; it keeps err for errors.Is.
func hinted(err error, hint string) error {
	return &hintError{err: err, hint: hint}
}

type hintError struct {
	err  error
	hint string
}

func (e *hintError) Error() string { return plainError(e.err) + "。" + e.hint }
func (e *hintError) Unwrap() error { return e.err }

// toastOf is the toast of a refused change.
func toastOf(err error) string {
	var h *hintError
	if errors.As(err, &h) {
		return h.Error()
	}
	return plainError(err)
}

// iconTitles name the icons the apps draw, for the icon choice of a type. An icon missing here is offered by its
// own name.
var iconTitles = map[string]string{
	"plugin": "插件", "chart": "图表", "gauge": "仪表", "list": "清单", "bell": "铃铛", "doc": "文档", "calendar": "日历",
	"money": "钱", "extension": "拼图", "person": "人", "chat": "聊天", "mail": "邮件", "link": "链接", "folder": "文件夹",
	"star": "星星", "check": "打勾", "warning": "提醒", "clock": "时钟", "tag": "标签", "search": "放大镜",
	"settings": "齿轮", "branch": "分叉", "commit": "节点", "code": "代码", "pull_request": "合并请求", "merge": "合并",
	"file": "文件", "home": "家", "sofa": "沙发", "light": "灯", "power": "电源", "fan": "风扇", "thermometer": "温度计",
	"droplet": "水滴", "lock": "锁", "unlock": "开锁", "door": "门", "window": "窗户", "camera": "相机", "video": "视频",
	"play": "播放", "pause": "暂停", "stop": "停止", "speaker": "音箱", "music": "音乐", "tv": "电视", "plug": "插头",
	"battery": "电池", "bolt": "闪电", "sun": "太阳", "moon": "月亮", "cloud": "云", "rain": "雨", "snow": "雪",
	"wind": "风", "leaf": "叶子", "car": "车", "location": "定位", "robot": "机器人", "wifi": "无线网", "shield": "盾牌",
	"trend_up": "上升", "trend_down": "下降", "wallet": "钱包", "bank": "银行", "heart": "爱心", "plus": "加号",
	"minus": "减号", "trash": "垃圾桶", "edit": "笔", "refresh": "刷新", "share": "分享", "info": "说明",
}

// iconTitle is the name of an icon on the pages.
func iconTitle(name string) string {
	if s, ok := iconTitles[name]; ok {
		return s
	}
	return name
}
