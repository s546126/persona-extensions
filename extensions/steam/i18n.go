package main

import (
	"fmt"
	"strings"
)

type text struct {
	storeLanguage                                                       string
	invalidPage, invalidAction                                          string
	overview, status, refresh, back, details, store                     string
	recent, recentTime, totalTime, owned, library, wishlist, discounted string
	search, sort, byPlaytime, byName, previous, next, showing, capped   string
	noRecent, noGames, noMatches, missingGame, noWishes                 string
	noPrice, priceUnchecked                                             string
	achievements, noAchievements, unlocked, game, playing, unknown      string
	minutes, hoursMinutes, discount, atLeast, stale                     string
	qrHint, keyMode, keySignOut                                         string
	friendsPlaying, noFriendsPlaying, moreFriendsPlaying, unknownFriend string
	invalidFriendSelection                                              string
	states                                                              map[string]string
	problems                                                            map[problem]string

	recentNews, noRecentNews string
	backlog                  backlogText
	recap                    recapText
	memory                   memoryText

	inboxPrice, inboxGiveaway, inboxRelease, inboxDemo, inboxRoundup string
	inboxGiveawayHint, inboxReleaseHint, inboxDemoHint               string
	inboxThreshold, inboxTarget, inboxNewLow, inboxUnits, inboxTop   string
}

type recapText struct {
	title, period, invalidArgs, noBaseline, window string
	topGames, addedTime, newGames, annotations     string
	progress, backlog, movedInto, consumption      string
	periods                                        map[recapPeriod]string
}

type memoryText struct {
	favorites, genres, backlog, annotations, playing, recent string
	milestone, completed, achievements, hours, separator     string
}

type backlogText struct {
	title, annotation, status, note, tags, unmarked, overlap         string
	save, clear, confirmClear, saved, cleared, accountChanged        string
	activity                                                         map[string]string
	statuses                                                         map[BacklogStatus]string
	pickFamiliar, pickRecent, pickTime, pickPlaying, pickQueued      string
	pickPaused, pickUnplayed, pickTried, pickLongTerm, pickCompleted string
}

func (t *backlogText) bucketTitle(bucket string) string {
	if title := t.activity[bucket]; title != "" {
		return title
	}
	return t.statuses[BacklogStatus(bucket)]
}

var english = text{
	storeLanguage: "english",
	invalidPage:   "Invalid Steam page state.", invalidAction: "Invalid Steam page action.",
	overview: "Steam overview", status: "Player status", refresh: "Refresh", back: "Back",
	details: "Game details", store: "Open in Steam Store",
	recent: "Recently played", recentTime: "Last 2 weeks", totalTime: "Total playtime", owned: "Owned games",
	library: "Game library", wishlist: "Wishlist", discounted: "Wishlist discounts",
	search: "Search your library by name or AppID", sort: "Sort library", byPlaytime: "Playtime", byName: "Name",
	previous: "Previous", next: "Next", showing: "%d–%d of %d", capped: "This snapshot includes %d of %d titles.",
	noRecent: "No games played in the last two weeks.", noGames: "No owned games are visible.", noMatches: "No games match this search.",
	missingGame: "This game is unavailable in the current list.",
	noWishes:    "The wishlist is empty.", noPrice: "No price listed in this region.", priceUnchecked: "Price unavailable.",
	achievements: "Achievements", noAchievements: "Steam lists no achievements for this game.", unlocked: "%d of %d unlocked",
	game: "App %d", playing: "Playing %s", unknown: "Status unavailable",
	recentNews: "Recent official updates", noRecentNews: "No recent official game updates were found.",
	recap: recapText{
		title: "Playtime recap", period: "Period", invalidArgs: "Use month, quarter, year or all, with top_n from 1 to 20.",
		noBaseline: "This period needs a start snapshot. Open the recap on another day to track playtime changes automatically.",
		window:     "Observed %s through %s (UTC).", topGames: "Most played in this period", addedTime: "Playtime added", newGames: "New in library",
		annotations: "Current local annotations", progress: "Backlog progress", backlog: "Backlog", movedInto: "%d moved in", consumption: "Backlog started or completed",
		periods: map[recapPeriod]string{recapMonth: "Monthly", recapQuarter: "Quarterly", recapYear: "Yearly", recapAll: "All observed history"},
	},
	memory: memoryText{
		favorites: "Most-played Steam games include %s.", genres: "Frequently played Steam genres include %s, based on available Store details.",
		backlog:     "Visible Steam library has %d waiting or paused, %d marked playing, %d completed, and %d dropped.",
		annotations: "Local Steam annotations mark %d waiting or paused, %d playing, %d completed, and %d dropped.",
		playing:     "Currently working through %s on Steam.", recent: "Recently played %s on Steam.",
		milestone: "Steam milestone for %s (%s).", completed: "marked completed", achievements: "%d/%d achievements unlocked", hours: "%d h played", separator: ", ",
	},
	inboxPrice: "Wishlist price alert", inboxGiveaway: "100% off", inboxRelease: "Released on Steam", inboxDemo: "Demo available",
	inboxRoundup: "Top wishlist deals", inboxGiveawayHint: "Check the Steam Store page to claim this limited-time offer.",
	inboxReleaseHint: "This wishlist game has launched. Open its Steam Store page for details.", inboxDemoHint: "Open the Steam Store page to try the free demo.",
	inboxThreshold: "At least %d%% off", inboxTarget: "Target price reached", inboxNewLow: "New locally observed low",
	inboxUnits: "%d Steam price units in %s", inboxTop: "%d wishlist games on sale in %s. Top %d offers.",
	minutes: "%d min", hoursMinutes: "%d h %d min", discount: "%d%% off", atLeast: "%d+", stale: "Showing cached data.",
	qrHint:         "Open Steam Guard in the Steam Mobile App, scan the QR code, and approve the sign-in on your phone.",
	keyMode:        "Web API key mode is managed in Steam's extension settings. Select Steam Mobile QR there to sign in by scanning.",
	keySignOut:     "To disconnect Web API key mode, clear the saved key in Steam's extension settings.",
	friendsPlaying: "Friends in game", noFriendsPlaying: "No friends with visible in-game activity.",
	moreFriendsPlaying: "More friends are playing.", unknownFriend: "Steam friend",
	invalidFriendSelection: "Select a friend by 17-digit SteamID64 or exact nickname, or provide 1–5 distinct SteamID64s.",
	states: map[string]string{
		"offline": "Offline", "online": "Online", "busy": "Busy", "away": "Away",
		"snooze": "Snoozing", "looking_to_trade": "Looking to trade", "looking_to_play": "Looking to play", "invisible": "Invisible",
	},
	problems: map[problem]string{
		problemSetup:              "Enter your Steam player ID and Web API key in Settings › Extensions › Steam.",
		problemInvalidID:          "Steam could not find this player. Check the SteamID64 or profile vanity name.",
		problemCountry:            "Enter an assigned two-letter ISO 3166-1 country code for the Steam Store.",
		problemMode:               "Choose Steam Mobile QR or Web API key in Steam's extension settings.",
		problemKey:                "Steam rejected the Web API key. Check the saved secret in Settings › Extensions › Steam.",
		problemPrivate:            "This Steam profile is private. Make the profile public to see its activity.",
		problemProfileUnavailable: "Steam did not share this player's profile.",
		problemGameDetails:        "Steam did not share these games. Availability depends on the sign-in method and Steam privacy settings.",
		problemAchievements:       "Steam did not share achievement progress. Availability depends on Steam privacy settings and whether the game supports achievements.",
		problemWishlist:           "Steam did not share a wishlist. It may be private, empty or unavailable.",
		problemMissing:            "Steam did not return this item for the selected Store country.",
		problemRateLimited:        "Steam is rate limiting requests. Try again in a moment.",
		problemTimeout:            "Steam took too long to respond. Try again in a moment.",
		problemNetwork:            "Steam is temporarily unavailable. Try again in a moment.",
		problemResponse:           "Steam returned an unexpected response. Try again in a moment.",
		problemCanceled:           "The Steam request was canceled.",
		problemAuthRequired:       "Sign in with the Steam Mobile App in Settings › Extensions › Steam.",
		problemAuthPending:        "Scan the QR code and approve the sign-in on your phone.",
		problemAuthRefused:        "Steam ended the sign-in request. Start a new QR sign-in.",
		problemAuthTimeout:        "The Steam QR sign-in expired. Start a new QR sign-in.",
		problemAuthExpired:        "Your Steam sign-in expired. Scan a new QR code to sign in again.",
		problemAuthLoad:           "Persona could not restore the saved Steam sign-in. Sign in with a new QR code.",
		problemAuthSave:           "Persona could not save the Steam sign-in. Try again when the Hub's secret store is available.",
		problemAuthDelete:         "Persona could not delete the saved Steam sign-in. Try signing out again.",
		problemAuthClearing:       "Persona is clearing the saved Steam sign-in. Try again in a moment.",
		problemBacklogStore:       "Persona could not access your local Steam backlog.",
		problemRecapStore:         "Persona could not access your local playtime history.",

		problemGlobalAchievements:     "Steam did not share global unlock rates for this game.",
		problemAchievementSchema:      "Steam did not share this game's achievement names and hidden status.",
		problemAchievementDetails:     "Steam did not share complete personal achievement details. Any global rankings are for all players.",
		problemAchievementUnlockTimes: "Steam did not share timestamps for some unlocked achievements.",
		problemGameNews:               "Steam did not share news for this game.",

		problemDiscoveryDisabled: "Steam game research is off (discovery_tools_enabled). You can enable it in Settings › Extensions › Steam.",
		problemDiscoveryTagArgs:  "Use a Steam language code, a tag query of up to 100 characters and a limit from 1 to 50.",
		problemDiscoverySearch:   "Use a valid search sort and language, up to eight tags, supported platforms and a nonnegative regional budget with up to two decimal places.",
		problemDiscoveryDetails:  "Provide one appid or a list of up to %d positive appids, with an optional Steam language code.",
		problemUnknownStoreTag:   "One or more Steam tags could not be resolved. Look up the exact names with steam_store_tags or use numeric tag IDs.",
		problemBuiltInTags:       "Using the built-in list of common Steam tags.",
		problemStoreSearchFailed: "Steam Store search is unavailable and there is no usable cached result for these filters. Try again shortly.",
		problemPartialLibrary:    "Steam returned only part of the library. Ownership is unknown for titles outside that snapshot.",

		problemFriendsDisabled:  "Friends Radar is off by default. Enable it in the Steam extension settings.",
		problemFriendList:       "Steam did not share the friends list. Check Friends List visibility in Steam and the saved Web API key in the extension settings.",
		problemFriendListQR:     "Steam did not share the friends list with this Mobile QR session. Check Friends List visibility in Steam. Web API key mode can read a public list.",
		problemFriendState:      "Steam did not share friend presence in Web API key mode. Check the key and profile visibility in Steam.",
		problemFriendStateQR:    "Steam did not grant this Mobile QR session access to friend presence. Web API key mode can read public presence.",
		problemFriendListCap:    "The friends radar checked up to 2,000 friends.",
		problemFriendUnverified: "The visible friends list did not verify one or more selected SteamIDs. Their libraries were skipped.",
		problemFriendName:       "No visible friend has that exact nickname. Check the name or use their SteamID64.",
		problemFriendNameDup:    "More than one visible friend uses that nickname. Select a friend by SteamID64.",
		problemFriendLibrary:    "Steam did not share one or more selected friends' game libraries. Check their Game details visibility. The intersection uses the libraries Steam shared.",
		problemFriendLibraryQR:  "Steam did not share a game library with this Mobile QR session. Its Game details may be private or restricted for this session.",
		problemFriendLibraryCap: "The intersection uses up to 20,000 visible games per account. One or more libraries are incomplete.",
		problemCoopStore:        "Some Store categories could not be checked. Games with unknown multiplayer support are included.",
	},
	backlog: backlogText{
		title: "Backlog & local progress", annotation: "Local backlog annotation", status: "Status",
		note: "Note", tags: "Tags", unmarked: "No local annotation", overlap: "A game can count in more than one category.",
		save: "Save status locally", clear: "Clear local annotation", confirmClear: "Clear this game's local status, note and tags?",
		saved: "Local Steam status saved.", cleared: "Local Steam annotation cleared.",
		accountChanged: "Your Steam account changed. Reopen this game to edit its annotation.",
		activity: map[string]string{
			neverPlayed: "Never played", triedUnder2h: "Tried under 2 hours",
			string(BacklogPlaying): "Playing or recently active", longTerm: "Played 20 hours or more",
		},
		statuses: map[BacklogStatus]string{
			BacklogUnplayed: "Unplayed", BacklogPlaying: "Playing", BacklogQueued: "Queued",
			BacklogPaused: "Paused", BacklogCompleted: "Completed", BacklogDropped: "Dropped",
		},
		pickFamiliar: "You have played it before.", pickRecent: "You played it in the last two weeks.",
		pickTime: "Its recorded playtime fits your time budget.", pickPlaying: "You marked it as playing.",
		pickQueued: "You queued it to play next.", pickPaused: "You marked it as paused.",
		pickUnplayed: "An unopened game from your library.", pickTried: "You have spent under two hours on it.",
		pickLongTerm: "You have spent at least 20 hours on it.", pickCompleted: "You previously marked it as completed.",
	},
}

var chinese = text{
	storeLanguage: "schinese",
	invalidPage:   "Steam 页面状态无效。", invalidAction: "Steam 页面操作无效。",
	overview: "Steam 概况", status: "玩家状态", refresh: "刷新", back: "返回",
	details: "游戏详情", store: "打开 Steam 商店",
	recent: "最近游玩", recentTime: "近两周", totalTime: "总游玩时长", owned: "拥有游戏",
	library: "游戏库", wishlist: "愿望单", discounted: "愿望单打折",
	search: "按名称或 AppID 搜索游戏库", sort: "游戏库排序", byPlaytime: "游玩时长", byName: "名称",
	previous: "上一页", next: "下一页", showing: "第 %d–%d 项，共 %d 项", capped: "此快照包含 %d 款游戏，共 %d 款。",
	noRecent: "近两周没有游玩记录。", noGames: "没有可见的游戏。", noMatches: "没有符合搜索条件的游戏。",
	missingGame: "当前列表中没有这款游戏。",
	noWishes:    "愿望单为空。", noPrice: "此地区没有挂牌价格。", priceUnchecked: "暂时无法获取价格。",
	achievements: "成就", noAchievements: "Steam 没有列出这款游戏的成就。", unlocked: "已解锁 %d / %d",
	game: "应用 %d", playing: "游戏中 · %s", unknown: "状态不可用",
	recentNews: "近期官方更新", noRecentNews: "暂未找到近期官方游戏更新。",
	recap: recapText{
		title: "游玩复盘", period: "周期", invalidArgs: "请选择 month、quarter、year 或 all，top_n 可设为 1–20。",
		noBaseline: "尚缺可对比的周期起点快照。改天再打开复盘，即可自动累计游玩增量。",
		window:     "观测区间为 %s 至 %s（UTC）。", topGames: "周期内最常玩", addedTime: "新增游玩时长", newGames: "新入库游戏",
		annotations: "当前本机标注", progress: "吃灰消化与进度", backlog: "待消化", movedInto: "%d 款移入", consumption: "吃灰消化率",
		periods: map[recapPeriod]string{recapMonth: "月度", recapQuarter: "季度", recapYear: "年度", recapAll: "全部"},
	},
	memory: memoryText{
		favorites: "Steam 最常玩的游戏包括 %s。", genres: "根据已有商店资料，Steam 常玩的游戏类型包括 %s。",
		backlog:     "可见 Steam 游戏库中有 %d 款待玩或暂停、%d 款标注为正在推进、%d 款已通关、%d 款已搁置。",
		annotations: "Steam 本机标注中有 %d 款待玩或暂停、%d 款正在推进、%d 款已通关、%d 款已搁置。",
		playing:     "正在推进 Steam 游戏《%s》。", recent: "近两周玩过 Steam 游戏《%s》。",
		milestone: "Steam 游戏《%s》的里程碑（%s）。", completed: "已标注通关", achievements: "已解锁 %d/%d 项成就", hours: "已玩 %d 小时", separator: "、",
	},
	inboxPrice: "愿望单价格提醒", inboxGiveaway: "喜加一", inboxRelease: "Steam 已发售", inboxDemo: "免费试玩已开放",
	inboxRoundup: "愿望单大促精选", inboxGiveawayHint: "前往 Steam 商店页面查看并领取限时免费优惠。",
	inboxReleaseHint: "这款愿望单游戏已发售，可前往 Steam 商店查看。", inboxDemoHint: "前往 Steam 商店页面下载免费试玩。",
	inboxThreshold: "折扣达到 %d%%", inboxTarget: "已达到目标价", inboxNewLow: "本机观测新低",
	inboxUnits: "%d Steam 价格单位（%s）", inboxTop: "%d 款愿望单游戏在 %s 区打折，精选 %d 款。",
	minutes: "%d 分钟", hoursMinutes: "%d 小时 %d 分钟", discount: "优惠 %d%%", atLeast: "%d+", stale: "当前显示缓存数据。",
	qrHint:         "在 Steam 手机 App 中打开 Steam 令牌，扫描二维码，并在手机上批准登录。",
	keyMode:        "Web API 密钥在 Steam 扩展设置中管理。如需扫码，请在设置中选择 Steam 手机 App 扫码。",
	keySignOut:     "如需断开 Web API 密钥模式，请在 Steam 扩展设置中清除已保存的密钥。",
	friendsPlaying: "好友在玩", noFriendsPlaying: "当前没有好友公开显示正在游玩。",
	moreFriendsPlaying: "还有其他好友正在游玩。", unknownFriend: "Steam 好友",
	invalidFriendSelection: "请用单个 17 位 SteamID64 或完整昵称指定好友，也可提供 1–5 个不重复的 SteamID64。",
	states: map[string]string{
		"offline": "离线", "online": "在线", "busy": "忙碌", "away": "离开",
		"snooze": "暂离", "looking_to_trade": "想交易", "looking_to_play": "想玩游戏", "invisible": "隐身",
	},
	problems: map[problem]string{
		problemSetup:              "请在「设置 › 扩展 › Steam」填写玩家标识和 Web API 密钥。",
		problemInvalidID:          "Steam 未找到该玩家，请检查 SteamID64 或个人资料自定义名称。",
		problemCountry:            "请填写有效的两位 ISO 3166-1 商店国家或地区代码。",
		problemMode:               "请在 Steam 扩展设置中选择手机 App 扫码或 Web API 密钥。",
		problemKey:                "Steam 拒绝了 Web API 密钥，请在「设置 › 扩展 › Steam」检查已保存的密钥。",
		problemPrivate:            "此 Steam 个人资料为私密状态。公开资料后可查看游戏活动。",
		problemProfileUnavailable: "Steam 未共享该玩家的个人资料。",
		problemGameDetails:        "Steam 未共享这些游戏。可见范围取决于登录方式和 Steam 隐私设置。",
		problemAchievements:       "Steam 未共享成就进度。可见范围取决于 Steam 隐私设置及游戏是否支持成就。",
		problemWishlist:           "Steam 未共享愿望单。愿望单可能为私密、为空或暂不可访问。",
		problemMissing:            "Steam 未返回所选商店地区的此项目。",
		problemRateLimited:        "Steam 正在限制请求频率，请稍后重试。",
		problemTimeout:            "Steam 响应超时，请稍后重试。",
		problemNetwork:            "Steam 暂时不可用，请稍后重试。",
		problemResponse:           "Steam 返回了无法识别的数据，请稍后重试。",
		problemCanceled:           "Steam 请求已取消。",
		problemAuthRequired:       "请在「设置 › 扩展 › Steam」使用 Steam 手机 App 扫码登录。",
		problemAuthPending:        "请扫描二维码，并在手机上批准登录。",
		problemAuthRefused:        "Steam 已结束此次登录请求，请重新发起扫码登录。",
		problemAuthTimeout:        "Steam 扫码登录已超时，请重新发起扫码登录。",
		problemAuthExpired:        "Steam 登录已失效，请重新扫码登录。",
		problemAuthLoad:           "Persona 无法恢复已保存的 Steam 登录，请重新扫码登录。",
		problemAuthSave:           "Persona 无法保存 Steam 登录，请在 Hub 密钥存储可用后重试。",
		problemAuthDelete:         "Persona 无法删除已保存的 Steam 登录，请重试登出。",
		problemAuthClearing:       "Persona 正在清除已保存的 Steam 登录，请稍后重试。",
		problemBacklogStore:       "Persona 无法访问本机 Steam 游戏标注。",
		problemRecapStore:         "Persona 无法访问本机 Steam 游玩历史。",

		problemGlobalAchievements:     "Steam 未共享这款游戏的全球成就达成率。",
		problemAchievementSchema:      "Steam 未共享这款游戏的成就名称和隐藏状态。",
		problemAchievementDetails:     "Steam 未共享完整的个人成就明细。全球榜单适用于所有玩家。",
		problemAchievementUnlockTimes: "Steam 未共享部分已解锁成就的解锁时间。",
		problemGameNews:               "Steam 未共享这款游戏的更新消息。",

		problemDiscoveryDisabled: "Steam 游戏调研已关闭（discovery_tools_enabled）。可在「设置 › 扩展 › Steam」开启。",
		problemDiscoveryTagArgs:  "请使用 Steam 语言代码，输入最多 100 字的标签关键词，并将返回上限设为 1–50。",
		problemDiscoverySearch:   "请使用有效的排序、语言和平台，提供最多 8 个标签，以及最多两位小数的非负地区预算。",
		problemDiscoveryDetails:  "请提供一个 appid，或最多 %d 个正整数 appids，可选填 Steam 语言代码。",
		problemUnknownStoreTag:   "无法识别一个或多个 Steam 标签。请用 steam_store_tags 查询准确名称，或直接提供标签 ID。",
		problemBuiltInTags:       "当前使用内置的 Steam 常用标签。",
		problemStoreSearchFailed: "Steam 商店搜索暂不可用，且这组筛选条件没有可用的缓存结果。请稍后重试。",
		problemPartialLibrary:    "Steam 只返回了部分游戏库，无法确认快照以外游戏的拥有状态。",

		problemFriendsDisabled:  "好友开黑雷达默认关闭，请在 Steam 扩展设置中开启。",
		problemFriendList:       "Steam 未共享好友列表。请检查 Steam 的好友列表隐私设置和扩展的 Web API 密钥。",
		problemFriendListQR:     "Steam 未向此次手机扫码会话共享好友列表。请检查 Steam 的好友列表隐私设置。公开列表可通过 Web API 密钥模式读取。",
		problemFriendState:      "Steam 未通过 Web API 密钥模式共享好友状态。请检查密钥和 Steam 资料的可见性。",
		problemFriendStateQR:    "Steam 未授权此次手机扫码会话读取好友状态。公开状态可通过 Web API 密钥模式读取。",
		problemFriendListCap:    "好友开黑雷达最多检查 2,000 位好友。",
		problemFriendUnverified: "可见的好友列表无法确认部分所选 SteamID 的好友身份，已跳过其游戏库。",
		problemFriendName:       "可见好友中没有完全匹配的昵称。请检查昵称，或使用好友的 SteamID64。",
		problemFriendNameDup:    "多位可见好友使用相同的昵称，请使用 SteamID64 指定好友。",
		problemFriendLibrary:    "Steam 未共享部分所选好友的游戏库。请检查对方的「游戏详情」可见性。交集使用 Steam 已共享的游戏库。",
		problemFriendLibraryQR:  "Steam 未向此次手机扫码会话共享某个游戏库。其「游戏详情」可能为私密，或此次会话权限受限。",
		problemFriendLibraryCap: "每个账号最多使用 20,000 款可见游戏计算交集，部分游戏库数据不完整。",
		problemCoopStore:        "暂时无法确认部分商店类别，结果保留联机支持情况未知的游戏。",
	},
	backlog: backlogText{
		title: "吃灰盘点与标注", annotation: "本机游戏标注", status: "游玩状态",
		note: "备注", tags: "标签", unmarked: "暂无本机标注", overlap: "同一款游戏可能计入多个分类。",
		save: "保存到本机", clear: "清除本机标注", confirmClear: "清除这款游戏的本机状态、备注和标签？",
		saved: "已保存本机 Steam 游戏状态。", cleared: "已清除本机 Steam 游戏标注。",
		accountChanged: "Steam 账号已切换，请重新打开游戏详情再编辑标注。",
		activity: map[string]string{
			neverPlayed: "从未打开", triedUnder2h: "试水不足 2 小时",
			string(BacklogPlaying): "正在推进或近两周活跃", longTerm: "游玩至少 20 小时",
		},
		statuses: map[BacklogStatus]string{
			BacklogUnplayed: "未玩", BacklogPlaying: "正在推进", BacklogQueued: "待玩",
			BacklogPaused: "暂停", BacklogCompleted: "已通关", BacklogDropped: "已搁置",
		},
		pickFamiliar: "你之前玩过这款游戏。", pickRecent: "你在近两周玩过这款游戏。",
		pickTime: "已有游玩时长在你今晚的时间预算内。", pickPlaying: "你把这款游戏标记为正在推进。",
		pickQueued: "你已把这款游戏排入待玩清单。", pickPaused: "你把这款游戏标记为暂停。",
		pickUnplayed: "游戏库里还没打开过的一款游戏。", pickTried: "你在这款游戏上试水不足两小时。",
		pickLongTerm: "你已在这款游戏上投入至少 20 小时。", pickCompleted: "你之前把这款游戏标记为已通关。",
	},
}

func textFor(locale string) *text {
	language, _, _ := strings.Cut(strings.ToLower(strings.ReplaceAll(locale, "_", "-")), "-")
	if language == "" || language == "zh" {
		return &chinese
	}
	return &english
}

func (t *text) message(issue problem, stale bool) string {
	if issue == "" {
		return ""
	}
	message := t.problems[issue]
	if message == "" {
		message = t.problems[problemResponse]
	}
	if stale {
		return t.stale + " " + message
	}
	return message
}

func (t *text) presence(p *player) string {
	if p != nil {
		if p.Playing != "" {
			return fmt.Sprintf(t.playing, p.Playing)
		}
		if p.PlayApp != 0 {
			return fmt.Sprintf(t.playing, t.gameName(p.PlayApp, ""))
		}
		if state := t.states[p.State]; state != "" {
			return state
		}
	}
	return t.unknown
}

func (t *text) gameName(appID uint32, name string) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf(t.game, appID)
}

func (t *text) playtime(n uint64) string {
	if n < 60 {
		return fmt.Sprintf(t.minutes, n)
	}
	return fmt.Sprintf(t.hoursMinutes, n/60, n%60)
}
