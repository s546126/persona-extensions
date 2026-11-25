package main

import "strings"

// The hook's words, in the language the user wrote in (detectLanguage): Chinese, Japanese, or English for the rest.
// They are shown in Claude Code, as the reason of a block or as a system message.

type msgs struct {
	blockHeader, rewrite, giveupHint, stillNonEnglish, mismatch, matchOK, stopword, warn, giveupResult,
	giveupFailed, judgeDown, hintFallback, translationFailed string
}

var messages = map[string]msgs{
	"en": {
		blockHeader:       "English by Default: this prompt isn't in English, so it was held back.",
		rewrite:           "Say the same thing in English. It goes through once a model agrees the meaning matches.",
		giveupHint:        "Type giveup to see it in English and send the original.",
		stillNonEnglish:   "Still not English. Write this in English:\n\"{original}\"",
		mismatch:          "The meaning doesn't match yet (score {score}).\nHint: {hint}\nTry again in English, or type giveup.",
		matchOK:           "English by Default: the meaning matches (score {score}). The pair was saved.",
		stopword:          "English by Default: an urgent word, so it went through. The prompt was saved for later.",
		warn:              "English by Default: in English, that's: {english}",
		giveupResult:      "English by Default: in English, that's: {english}",
		giveupFailed:      "English by Default: the translation failed, so the original went through.",
		judgeDown:         "English by Default: the model couldn't check the rewrite, so it went through.",
		hintFallback:      "think about what is missing",
		translationFailed: "the translation failed",
	},
	"zh": {
		blockHeader:       "English by Default：这条提示不是英文，先拦下了。",
		rewrite:           "请用英文把同样的意思再说一遍，模型判断意思对上了就放行。",
		giveupHint:        "想放弃就输入 giveup，会给出英文说法并发出原文。",
		stillNonEnglish:   "还是不是英文。请用英文写出这句话：\n「{original}」",
		mismatch:          "意思还没对上（得分 {score}）。\n提示：{hint}\n继续用英文改，或输入 giveup 放弃。",
		matchOK:           "English by Default：意思对上了（得分 {score}），这组对照已记下。",
		stopword:          "English by Default：有紧急词，直接放行了，原文已记下，以后再练。",
		warn:              "English by Default：英文可以这么说：{english}",
		giveupResult:      "English by Default：英文可以这么说：{english}",
		giveupFailed:      "English by Default：翻译失败，已放行原文。",
		judgeDown:         "English by Default：模型没能判断这次改写，已放行。",
		hintFallback:      "再想想漏了什么",
		translationFailed: "翻译失败",
	},
	"ja": {
		blockHeader:       "English by Default：このプロンプトは英語ではないため、止めました。",
		rewrite:           "同じ内容を英語で書き直してください。意味が一致するとモデルが判断すれば通ります。",
		giveupHint:        "諦める場合は giveup と入力すると、英語の言い方を表示して原文を送ります。",
		stillNonEnglish:   "まだ英語ではありません。この文を英語で書いてください：\n「{original}」",
		mismatch:          "まだ意味が一致しません（スコア {score}）。\nヒント：{hint}\n英語で書き直すか、giveup と入力してください。",
		matchOK:           "English by Default：意味が一致しました（スコア {score}）。対訳を記録しました。",
		stopword:          "English by Default：緊急ワードがあるので通しました。原文は記録してあります。",
		warn:              "English by Default：英語ではこう言えます：{english}",
		giveupResult:      "English by Default：英語ではこう言えます：{english}",
		giveupFailed:      "English by Default：翻訳に失敗したため、原文のまま通しました。",
		judgeDown:         "English by Default：モデルが書き直しを判定できなかったため、通しました。",
		hintFallback:      "何が足りないか考えてみてください",
		translationFailed: "翻訳に失敗しました",
	},
}

func messagesFor(lang string) msgs {
	if m, ok := messages[lang]; ok {
		return m
	}
	return messages["en"]
}

// fill puts the values into a message's {name} slots.
func fill(s string, kv ...string) string {
	for i := 0; i+1 < len(kv); i += 2 {
		s = strings.ReplaceAll(s, "{"+kv[i]+"}", kv[i+1])
	}
	return s
}
