package main

import "strings"

// Page chrome follows the hub locale (zh-Hans, en, ja). An empty or unknown locale
// is zh-Hans, matching the hub's default. The article itself stays English.

type texts struct {
	title                           string
	modeTranslation, modeVocabulary string
	current, saved, save, close     string
	wordsTitle, savedTitle          string
	times, savedCount               string
	more, less                      string
	failed, failedBare              string
	attribution, reading            string
	intro                           string
}

var locales = map[string]*texts{
	"zh-Hans": {
		title: "英文阅读", modeTranslation: "翻译模式", modeVocabulary: "生词模式",
		current: "当前", saved: "已记下", save: "记下这个词", close: "收起",
		wordsTitle: "生词", savedTitle: "已记的词",
		times: "出现 %d 次", savedCount: "已记下 %d",
		more: "再显示一段", less: "收起新增的一段",
		failed: "这一段暂时没翻译出来，过一会儿再打开试试（原因：%s）", failedBare: "这一段暂时没翻译出来，过一会儿再打开试试",
		attribution: "释义来自 ECDICT（MIT，Linwei）。", reading: "正文",
		intro: "用一篇英文短文练阅读。「翻译模式」在每段原文下面附上中文；「生词模式」列出文中较难的词，点一个看意思，有用就记下来。记下的词会一直留着。",
	},
	"en": {
		title: "English reading", modeTranslation: "Translation", modeVocabulary: "Vocabulary",
		current: "On", saved: "Saved", save: "Save word", close: "Close",
		wordsTitle: "Words", savedTitle: "Saved words",
		times: "%d times", savedCount: "Saved %d",
		more: "Show another paragraph", less: "Hide the extra paragraph",
		failed: "This paragraph could not be translated right now. Try opening the page again later (reason: %s).", failedBare: "This paragraph could not be translated right now. Try opening the page again later.",
		attribution: "Glosses from ECDICT (MIT), Linwei.", reading: "Article",
		intro: "Practise reading with a short English article. Translation puts a translation under each paragraph; Vocabulary lists the harder words: tap one to see its meaning and keep the useful ones. Saved words stay until you remove them.",
	},
	"ja": {
		title: "英文リーディング", modeTranslation: "翻訳モード", modeVocabulary: "単語モード",
		current: "選択中", saved: "記録済み", save: "この単語を覚える", close: "閉じる",
		wordsTitle: "単語", savedTitle: "記録した単語",
		times: "%d 回", savedCount: "記録 %d",
		more: "もう一段表示", less: "追加の段を隠す",
		failed: "この段落は今は翻訳できませんでした。しばらくしてからもう一度開いてください（理由：%s）。", failedBare: "この段落は今は翻訳できませんでした。しばらくしてからもう一度開いてください。",
		attribution: "語義は ECDICT（MIT、Linwei）によります。", reading: "本文",
		intro: "短い英文で読む練習をします。「翻訳モード」は各段落の下に訳を付けます。「単語モード」は難しめの単語を並べます。タップすると意味が見られ、役に立つ単語は記録できます。記録した単語は消すまで残ります。",
	},
}

func textsFor(locale string) *texts {
	loc := strings.ToLower(strings.TrimSpace(locale))
	switch {
	case strings.HasPrefix(loc, "en"):
		return locales["en"]
	case strings.HasPrefix(loc, "ja"):
		return locales["ja"]
	default:
		return locales["zh-Hans"]
	}
}
