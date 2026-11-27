package main

import (
	"fmt"
	"strings"
)

// Sentences of the disk_offload point (offload.go) in the caller's language. The pages of this extension are in
// Chinese only; the point is drawn by Persona's own apps, which pass the user's locale with every call, so what it
// returns for people follows that locale: en, zh-Hans or ja, falling back to en.

const (
	locEN = "en"
	locZH = "zh-Hans"
	locJA = "ja"
)

// offloadLocale maps a BCP 47 tag to one of the three tables.
func offloadLocale(tag string) string {
	t := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(tag, "_", "-")))
	switch {
	case t == "zh" || strings.HasPrefix(t, "zh-"):
		return locZH
	case t == "ja" || strings.HasPrefix(t, "ja-"):
		return locJA
	}
	return locEN
}

var offloadText = map[string]map[string]string{
	locEN: {
		"unknown_machine":  "%s is not one of the computers Disklessly looks after. Add it on the Disk space page, or as a Host in ~/.ssh/config on the computer that runs Persona.",
		"offline":          "%s is offline.",
		"unreachable":      "Could not reach %s: %s",
		"windows":          "Moving items to the network drive is not available on Windows computers.",
		"no_cli":           "Disklessly's helper tool (disklessly) is not installed on %s.",
		"cli_old":          "disklessly %s on %s is too old: it cannot offload files and folders yet.",
		"no_backend":       "disklessly on %s has no network drive set up yet. Run disklessly init (or disklessly backend add) on that computer first.",
		"unreadable":       "disklessly on %s gave an answer that could not be read: %s",
		"partial":          "Some of what is on the network drive could not be listed.",
		"tool_title":       "Disklessly helper tool",
		"source_brew":      "Installs disklessly-cli from the Homebrew tap nwverse/tap with Homebrew on %s, into Homebrew's own folder; nothing else is changed.",
		"source_copy":      "Copies disklessly %s from %s to ~/.local/opt/%s on %s and links ~/.local/bin/disklessly to it; nothing is downloaded and no administrator rights are needed.",
		"source_none":      "It cannot be installed automatically: %s has no Homebrew and no computer of the same kind has a copy to share. Install it there with: brew tap nwverse/tap && brew install nwverse/tap/disklessly-cli",
		"no_install_route": "disklessly cannot be installed on %s automatically. Install it there with: brew tap nwverse/tap && brew install nwverse/tap/disklessly-cli",
		"busy":             "%s is busy with another Disklessly task; try again when it finishes.",
		"install_failed":   "Installing disklessly failed: %s",
		"still_old":        "disklessly was installed, but the version on %s still cannot offload files and folders.",
		"all_failed":       "Nothing was moved: %s",
		"canceled":         "Canceled; items not started were left as they were.",
		"no_items":         "Nothing to do.",
		"bad_action":       "Choose either move to the network drive (offload) or bring back (restore).",
		// Refusals, by code.
		"refusal_bad_path":          "Not an absolute path.",
		"refusal_not_found":         "It does not exist.",
		"refusal_symlink":           "It is a symbolic link; offload what it points to instead.",
		"refusal_not_regular":       "It is not a regular file or folder.",
		"refusal_outside_home":      "Only items in the home folder can be offloaded.",
		"refusal_home_root":         "The home folder itself cannot be offloaded.",
		"refusal_standard_folder":   "Standard folders such as Documents or Library stay; offload what is inside them.",
		"refusal_app_managed":       "Its app manages it (Library, Applications, Trash, Public, app libraries and virtual machine disks stay).",
		"refusal_inside_app":        "It is inside an app bundle.",
		"refusal_own_data":          "It holds Disklessly's own storage, cache or state.",
		"refusal_credentials":       "Keys and credentials stay on the computer.",
		"refusal_placeholder":       "It is already a Disklessly placeholder.",
		"refusal_placeholder_taken": "A Disklessly placeholder with that name is already next to it; bring that one back first.",
		"refusal_already_offloaded": "This app is already on the network drive.",
		"refusal_app_platform":      "Apps can be offloaded on Mac computers only.",
		"refusal_not_app":           "It is not an app bundle.",
		"refusal_kind":              "Disklessly does not offload this kind of item.",
		"refusal_unknown_item":      "It is no longer in the network drive's list; refresh and try again.",
		"refusal_not_restorable":    "It cannot be brought back from this computer.",
		"refusal_tool":              "The helper tool did not move it (it said: %s).",
	},
	locZH: {
		"unknown_machine":           "「%s」不在 Disklessly 管的电脑里。请在「硬盘空间」页添加，或者在运行 Persona 的电脑的 ~/.ssh/config 里加一个 Host。",
		"offline":                   "「%s」连不上（可能关机了）。",
		"unreachable":               "连不上「%s」：%s",
		"windows":                   "Windows 电脑暂时不能搬到网盘。",
		"no_cli":                    "「%s」上还没装配套小工具（disklessly）。",
		"cli_old":                   "「%[2]s」上的配套小工具 disklessly %[1]s 太旧，还不能把文件和文件夹搬到网盘。",
		"no_backend":                "「%s」上的配套小工具还没有设置网盘。请先在那台电脑上运行 disklessly init（或 disklessly backend add）。",
		"unreadable":                "「%s」上的配套小工具出错了（回答看不懂）：%s",
		"partial":                   "有一部分已经在网盘里的内容没能列出来。",
		"tool_title":                "Disklessly 配套小工具",
		"source_brew":               "在「%s」上用 Homebrew 从 nwverse/tap 安装 disklessly-cli，装在 Homebrew 自己的目录里，别的什么都不改。",
		"source_copy":               "把配套小工具 disklessly %s 从「%s」复制到「%[4]s」的 ~/.local/opt/%[3]s，并把 ~/.local/bin/disklessly 指向它；不从网上下载，也不需要管理员权限。",
		"source_none":               "没法自动安装：「%s」上没有 Homebrew，也没有同类的电脑可以复制。请在那台电脑上运行：brew tap nwverse/tap && brew install nwverse/tap/disklessly-cli",
		"no_install_route":          "没法在「%s」上自动安装配套小工具。请在那台电脑上运行：brew tap nwverse/tap && brew install nwverse/tap/disklessly-cli",
		"busy":                      "「%s」正在忙另一件 Disklessly 的事，等它做完再试。",
		"install_failed":            "安装配套小工具没有成功：%s",
		"still_old":                 "配套小工具装好了，但「%s」上的版本还是不能把文件和文件夹搬到网盘。",
		"all_failed":                "什么都没有搬：%s",
		"canceled":                  "已取消，还没开始的项目都原样留着。",
		"no_items":                  "没有要做的。",
		"bad_action":                "只能选「搬到网盘」（offload）或「拿回来」（restore）。",
		"refusal_bad_path":          "不是绝对路径。",
		"refusal_not_found":         "它不存在。",
		"refusal_symlink":           "它是一个快捷方式（符号链接），请搬它指向的内容。",
		"refusal_not_regular":       "它不是普通文件或文件夹。",
		"refusal_outside_home":      "只能搬个人文件夹里的内容。",
		"refusal_home_root":         "不能把整个个人文件夹搬走。",
		"refusal_standard_folder":   "「文稿」「资源库」这些标准文件夹留在电脑上，请搬里面的内容。",
		"refusal_app_managed":       "它由所属的 App 自己管（资源库、应用程序、废纸篓、公共、App 资料库和虚拟机磁盘都留在电脑上）。",
		"refusal_inside_app":        "它在某个 App 包里面。",
		"refusal_own_data":          "里面有 Disklessly 自己的数据、缓存或状态。",
		"refusal_credentials":       "密钥和密码类的文件留在电脑上。",
		"refusal_placeholder":       "它已经是 Disklessly 留下的占位文件。",
		"refusal_placeholder_taken": "旁边已经有同名的 Disklessly 占位文件，请先把那一个拿回来。",
		"refusal_already_offloaded": "这个 App 已经在网盘里了。",
		"refusal_app_platform":      "只有 Mac 上的 App 能搬到网盘。",
		"refusal_not_app":           "它不是 App。",
		"refusal_kind":              "Disklessly 不搬这类东西。",
		"refusal_unknown_item":      "网盘清单里已经没有它了，请刷新后再试。",
		"refusal_not_restorable":    "没法从这台电脑把它拿回来。",
		"refusal_tool":              "配套小工具没有搬它（它说：%s）。",
	},
	locJA: {
		"unknown_machine":           "%s は Disklessly が管理するコンピュータではありません。「ディスク容量」ページで追加するか、Persona を動かしているコンピュータの ~/.ssh/config に Host を追加してください。",
		"offline":                   "%s はオフラインです。",
		"unreachable":               "%s に接続できません：%s",
		"windows":                   "Windows コンピュータではネットワークドライブへの移動を利用できません。",
		"no_cli":                    "%s に Disklessly の補助ツール（disklessly）がインストールされていません。",
		"cli_old":                   "%[2]s の disklessly %[1]s は古いため、ファイルとフォルダをまだ移動できません。",
		"no_backend":                "%s の disklessly にはまだネットワークドライブが設定されていません。先にそのコンピュータで disklessly init（または disklessly backend add）を実行してください。",
		"unreadable":                "%s の disklessly の応答を読み取れませんでした：%s",
		"partial":                   "ネットワークドライブにある項目の一部を一覧にできませんでした。",
		"tool_title":                "Disklessly 補助ツール",
		"source_brew":               "%s の Homebrew で nwverse/tap から disklessly-cli を Homebrew 自身のフォルダにインストールします。ほかには何も変更しません。",
		"source_copy":               "disklessly %s を %s から %[4]s の ~/.local/opt/%[3]s にコピーし、~/.local/bin/disklessly をそこにリンクします。ダウンロードも管理者権限も不要です。",
		"source_none":               "自動ではインストールできません：%s には Homebrew がなく、コピー元になる同じ種類のコンピュータもありません。そのコンピュータで次を実行してください：brew tap nwverse/tap && brew install nwverse/tap/disklessly-cli",
		"no_install_route":          "%s には disklessly を自動でインストールできません。そのコンピュータで次を実行してください：brew tap nwverse/tap && brew install nwverse/tap/disklessly-cli",
		"busy":                      "%s は別の Disklessly の処理中です。終わってからもう一度お試しください。",
		"install_failed":            "disklessly のインストールに失敗しました：%s",
		"still_old":                 "disklessly をインストールしましたが、%s のバージョンはまだファイルとフォルダを移動できません。",
		"all_failed":                "何も移動しませんでした：%s",
		"canceled":                  "キャンセルしました。未着手の項目はそのままです。",
		"no_items":                  "処理する項目がありません。",
		"bad_action":                "「ネットワークドライブへ移動」（offload）か「戻す」（restore）のどちらかを選んでください。",
		"refusal_bad_path":          "絶対パスではありません。",
		"refusal_not_found":         "存在しません。",
		"refusal_symlink":           "シンボリックリンクです。リンク先を移動してください。",
		"refusal_not_regular":       "通常のファイルやフォルダではありません。",
		"refusal_outside_home":      "ホームフォルダ内の項目だけを移動できます。",
		"refusal_home_root":         "ホームフォルダそのものは移動できません。",
		"refusal_standard_folder":   "書類やライブラリなどの標準フォルダは残します。中身を移動してください。",
		"refusal_app_managed":       "App 自身が管理しています（ライブラリ、アプリケーション、ゴミ箱、パブリック、App のライブラリや仮想マシンのディスクは残します）。",
		"refusal_inside_app":        "App バンドルの中にあります。",
		"refusal_own_data":          "Disklessly 自身の保存先、キャッシュ、状態を含みます。",
		"refusal_credentials":       "鍵や認証情報はコンピュータに残します。",
		"refusal_placeholder":       "すでに Disklessly のプレースホルダです。",
		"refusal_placeholder_taken": "同じ名前の Disklessly プレースホルダが隣にあります。先にそちらを戻してください。",
		"refusal_already_offloaded": "この App はすでにネットワークドライブにあります。",
		"refusal_app_platform":      "App を移動できるのは Mac だけです。",
		"refusal_not_app":           "App バンドルではありません。",
		"refusal_kind":              "Disklessly はこの種類の項目を扱いません。",
		"refusal_unknown_item":      "ネットワークドライブの一覧にもうありません。更新してからもう一度お試しください。",
		"refusal_not_restorable":    "このコンピュータからは戻せません。",
		"refusal_tool":              "補助ツールはこれを移動しませんでした（理由：%s）。",
	},
}

// otr is the sentence key in locale loc (en when that table lacks it), formatted with args.
func otr(loc, key string, args ...any) string {
	s, ok := offloadText[offloadLocale(loc)][key]
	if !ok {
		s = offloadText[locEN][key]
	}
	if s == "" {
		s = key
	}
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

// refusalText is the sentence for refusal code, with the tool's own words for "tool".
func refusalText(loc, code, detail string) string {
	if code == "tool" {
		return otr(loc, "refusal_tool", detail)
	}
	return otr(loc, "refusal_"+code)
}
