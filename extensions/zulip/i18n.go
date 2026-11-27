package main

import "strings"

type texts struct {
	inbox, streams, noStreams, topics, noTopics, emptyTopic, latest, messages, noMessages, more, back, missingConfig string
}

var locales = map[string]*texts{
	"en": {
		inbox: "Zulip unread", streams: "Subscribed streams", noStreams: "No accessible streams.", topics: "Topics", noTopics: "No topics in this stream.", emptyTopic: "(no topic)", latest: "Latest message ID: %d", messages: "Messages", noMessages: "No messages in this topic.", more: "Older messages", back: "Back", missingConfig: "Set the Zulip instance URL, API email and API key in Settings › Extensions first.",
	},
	"ja": {
		inbox: "Zulip 未読", streams: "購読中のストリーム", noStreams: "アクセス可能なストリームはありません。", topics: "トピック", noTopics: "このストリームにトピックはありません。", emptyTopic: "（トピックなし）", latest: "最新メッセージ ID: %d", messages: "メッセージ", noMessages: "このトピックにメッセージはありません。", more: "過去のメッセージ", back: "戻る", missingConfig: "設定 › 拡張機能で Zulip のインスタンス URL、API メール、API キーを設定してください。",
	},
	"zh-Hans": {
		inbox: "Zulip 未读话题", streams: "已订阅频道", noStreams: "没有可访问的频道。", topics: "话题", noTopics: "这个频道没有话题。", emptyTopic: "（无话题）", latest: "最新消息 ID：%d", messages: "消息", noMessages: "这个话题没有消息。", more: "更早的消息", back: "返回", missingConfig: "先在「设置 › 扩展」中填写 Zulip 实例地址、API 邮箱和 API key。",
	},
}

func textsFor(locale string) *texts {
	if t := locales[locale]; t != nil {
		return t
	}
	if i := strings.IndexAny(locale, "-_"); i > 0 {
		if t := locales[locale[:i]]; t != nil {
			return t
		}
	}
	return locales["en"]
}
