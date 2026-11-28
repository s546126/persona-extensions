package main

import "strings"

// text is everything the page says, in one language.
type text struct {
	title, machine, searchHint, listSep                                  string
	hubMachine                                                           string
	tabRepos, tabSetup, tabLSP                                           string
	noMachines, noMachinesHint, noComputer                               string
	repos, repoList, noRepos, noReposHint, noReposIn                     string
	lsp, noLsp, langMissing                                              string
	files, filesCut, noFiles, outline, noOutline, noServer, noServerHint string
	back, forward, backToCode, pickFile, pickFileHint, navHint           string
	noRead, binary, earlier, outside, fileCut                            string
	findRefs, gotoDef, gotoImpl, showHover                               string
	noSearch, noMatch, matches, symbolFallback, symbolFallbackLang       string
	noRefs, thisSymbol, refsOf, defsOf, implsOf, noneFound               string
	noDef, noDefHere, noImplHere                                         string
	gotoLine, gotoLineHint, gotoButton                                   string
	lineNames, lineAt, lineNoServer                                      string
	hoverOf, hoverNone, noHover, close                                   string
	scopeSymbols, scopeText                                              string
	grepTitle, grepNone, grepCut, grepPartial, noGrep, grepEmpty         string
	dirTitle, dirEmpty, folderWord                                       string
	symbolWord                                                           string

	// Onboarding / Setup UI
	setupHeadline, setupIntro                                                         string
	setupStepMachines, setupStepRoots, setupStepNext                                  string
	setupHubDesc, setupHubEnable, setupHubDisable                                     string
	setupNodeEnabled, setupNodePairedHint, setupOnline, setupOffline                  string
	setupRootLabel, setupRootPlaceholder, setupRootAdd, setupRootEmpty                string
	setupRootRemove, setupRootRemoveConfirm, setupRootSuggestions, setupAddSuggestion string
	setupRootOK, setupRootMissing, setupRootPerm, setupRootForbidden                  string
	setupRootNotDir, setupRootReposCount                                              string
	setupBadgeEnabled, setupBadgeDisabled, setupBadgeOK                               string
	setupBadgeMissing, setupBadgeDenied, setupBadgeBlocked                            string
	setupStatMachines, setupStatRoots, setupStatRepos, setupStatLSP                   string
	setupReprobe, setupToLSP, setupFinish, setupOpenBtn                               string
	setupNoTarget, setupNoTargetHint, setupSaved, setupRootInvalid                    string
	setupRootExists, setupRootLimit                                                   string

	// Per-machine LSP Config & Probe UI
	lspHeadline, lspSubtitle, lspReprobe, lspReapBtn, lspReapedToast string
	lspStatReady, lspStatRunning, lspStatIdle, lspStatRepos          string
	lspSectionList, lspSectionEdit, lspSectionIdle                   string
	lspBadgeReady, lspBadgeMissing, lspBadgeDisabled, lspBadgeBad    string
	lspRunningTag, lspCustomTag, lspAutoTag, lspNoInstallHint        string
	lspPathLabel, lspPathPlaceholder, lspSaveBtn, lspResetBtn        string
	lspEnableBtn, lspDisableBtn                                      string
	lspIdleLabel, lspIdleSaveBtn, lspIdleUnit, lspIdleInvalid        string
	lspSavedReady, lspSavedBadPath, lspSavedDisabled, lspSavedAuto   string
	lspIdleSaved, lspConfigBtn, lspNotReadyEmpty, lspNotReadyHint    string

	// Git Diff & Jump to Code UI
	diffBtn, diffCrumb, diffBackList, diffOpenSource                     string
	diffModeUnstaged, diffModeStaged, diffModeBranch                     string
	diffNotGit, diffNotGitHint, diffNoBase, diffNoBaseHint               string
	diffEmpty, diffEmptyHint, diffError, diffTruncated                   string
	diffSummaryBranch, diffSummaryBase, diffSummaryFiles                 string
	diffBadgeAdded, diffBadgeModified, diffBadgeDeleted, diffBadgeRename string
	diffUntracked, diffRenamedFrom, diffDeletedNoSrc, diffBinaryFile     string
	diffActionView, diffActionJump                                       string
	diffJumpTitle, diffJumpLineFmt, diffHunkBadgeAdd, diffHunkBadgeDel   string

	kinds   []string
	install map[string]string
}

// langTitle is how the page names a language.
var langTitle = map[string]string{
	"go": "Go", "typescript": "TypeScript", "javascript": "JavaScript", "python": "Python", "rust": "Rust",
	"swift": "Swift", "c": "C", "cpp": "C++",
}

// langGroupTitle names a configurable language group in the LSP manager.
var langGroupTitle = map[string]string{
	"go":         "Go",
	"typescript": "TypeScript / JavaScript",
	"python":     "Python",
	"rust":       "Rust",
	"swift":      "Swift",
	"c":          "C / C++",
}

func languageTitle(name string) string {
	if t, ok := langTitle[name]; ok {
		return t
	}
	return name
}

func languageGroupTitle(name string) string {
	if t, ok := langGroupTitle[canonicalLang(name)]; ok {
		return t
	}
	return languageTitle(name)
}

var zh = text{
	title: "代码", machine: "电脑", searchHint: "搜符号或文字…", listSep: "、",
	hubMachine: "本机（Hub）",
	tabRepos:   "仓库", tabSetup: "配置向导", tabLSP: "语言服务",
	noMachines: "还没选电脑", noMachinesHint: "在下方配置向导里启用本机（Hub）或在扩展设置勾选电脑，并添加代码文件夹（默认 ~/code）。",
	noComputer: "连不上这台电脑上的代码导航",
	repos:      "仓库（%d）", repoList: "仓库", noRepos: "没找到仓库",
	noReposHint: "点击「配置向导」添加代码文件夹并实时检测仓库。", noReposIn: "这些文件夹里没找到代码仓库：%s",
	lsp:         "能跳转的语言：%s",
	noLsp:       "这台电脑上还没装语言服务，现在可以看代码和全文搜索；切换到「语言服务」标签可检测与配置。",
	langMissing: "这个仓库里的 %s 代码暂时只能看、不能跳转。%s",
	files:       "文件（%d）", filesCut: "，只列了一部分", noFiles: "列不出文件", outline: "大纲",
	noOutline: "大纲出不来：", noServer: "这台电脑上没有 %s 的语言服务", noServerHint: "这台电脑上没有 %s 的语言服务，装上就能跳转。%s",
	back: "← 后退", forward: "前进 →", backToCode: "回到代码", pickFile: "选个文件看看",
	pickFileHint: "在文件树或上面的文件夹里点开文件；点大纲里的名字跳过去。",
	navHint:      "在代码里 ⌘ 点名字（或按 ⌘B）跳到定义，⌥F7 查找引用，⌘[ 后退。",
	noRead:       "打不开这个文件", binary: "这是二进制文件，不显示", earlier: "显示前面的 %d 行",
	outside: "这是仓库外的代码（标准库或依赖），只读。", fileCut: "文件太长，后面没读进来。",
	findRefs: "查找引用", gotoDef: "跳到定义", gotoImpl: "跳到实现", showHover: "查看说明",
	noSearch: "搜不了符号", noMatch: "没有叫「%s」的符号", matches: "%d 个符号匹配「%s」",
	symbolFallback:     "这个仓库没有能用的语言服务，下面是全文搜索的结果。",
	symbolFallbackLang: "这台电脑上没有 %s 的语言服务，下面是全文搜索的结果。",
	noRefs:             "查不了", thisSymbol: "这个符号", refsOf: "「%s」的引用（%d）", defsOf: "「%s」的定义（%d）",
	implsOf: "「%s」的实现（%d）", noneFound: "一个也没找到",
	noDef: "没能跳到定义：", noDefHere: "这里没找到定义。", noImplHere: "这里没找到实现。",
	gotoLine: "跳到行", gotoLineHint: "行号，或 行:列", gotoButton: "跳转",
	lineNames: "第 %d 行的名字", lineAt: "第 %d 行，第 %d 列",
	lineNoServer: "装上这种语言的服务后，点这一行里的名字就能跳转。",
	hoverOf:      "「%s」的说明", hoverNone: "这里没有说明。", noHover: "看不了说明", close: "关闭",
	scopeSymbols: "符号", scopeText: "全文",
	grepTitle: "「%s」在 %d 个文件里出现 %d 处", grepNone: "没找到「%s」", grepCut: "只列出了前 %d 处。",
	grepPartial: "文件太多，只搜了 %d 个文件。", noGrep: "全文搜索没成功", grepEmpty: "在上面的搜索框里输入要找的文字",
	dirTitle: "%s（%d 项）", dirEmpty: "这个文件夹是空的", folderWord: "文件夹",
	symbolWord: "符号",

	setupHeadline:          "初始配置向导",
	setupIntro:             "选择要看代码的电脑（Hub 本机或已配对节点）并管理代码根目录。全程严格只读，绝不改动任何仓库文件。",
	setupStepMachines:      "第 1 步 · 选择目标机器",
	setupStepRoots:         "第 2 步 · 管理代码根目录（%s）",
	setupStepNext:          "第 3 步 · 检测并开始使用",
	setupHubDesc:           "直接只读浏览运行 Persona Hub 的本机代码",
	setupHubEnable:         "启用本机（Hub）",
	setupHubDisable:        "停用本机（Hub）",
	setupNodeEnabled:       "已授权节点 · 点击切换管理其目录与语言服务",
	setupNodePairedHint:    "已配对节点；如需直连看代码，请在扩展设置勾选「在哪些电脑上看代码」",
	setupOnline:            "在线",
	setupOffline:           "离线",
	setupRootLabel:         "添加代码根目录",
	setupRootPlaceholder:   "例如 ~/code 或 /path/to/workspace",
	setupRootAdd:           "添加并检测",
	setupRootEmpty:         "还没有配置代码文件夹，可在下方输入路径或点选自动发现的目录。",
	setupRootRemove:        "移除",
	setupRootRemoveConfirm: "从代码导航中移除这个文件夹？（不会删除磁盘上的任何文件）",
	setupRootSuggestions:   "检测到以下常见代码目录，点击一键添加：",
	setupAddSuggestion:     "添加 %s（%d 个仓库）",
	setupRootOK:            "可读 · 发现 %d 个仓库",
	setupRootMissing:       "目录不存在",
	setupRootPerm:          "无读取权限",
	setupRootForbidden:     "受保护或敏感目录，已安全拦截",
	setupRootNotDir:        "路径不是文件夹",
	setupRootReposCount:    "%d 个仓库",
	setupBadgeEnabled:      "已启用",
	setupBadgeDisabled:     "未启用",
	setupBadgeOK:           "可用",
	setupBadgeMissing:      "不存在",
	setupBadgeDenied:       "无权限",
	setupBadgeBlocked:      "已拦截",
	setupStatMachines:      "已启用机器",
	setupStatRoots:         "代码目录",
	setupStatRepos:         "可用仓库",
	setupStatLSP:           "就绪语言",
	setupReprobe:           "重新检测",
	setupToLSP:             "配置语言服务",
	setupFinish:            "完成并浏览仓库",
	setupOpenBtn:           "打开配置向导",
	setupNoTarget:          "先启用一台电脑",
	setupNoTargetHint:      "点击上方「本机（Hub）」一键启用，或在扩展设置里勾选远程电脑。",
	setupSaved:             "配置已保存并完成检测",
	setupRootInvalid:       "目录路径无效，请输入绝对路径或以 ~/ 开头的路径",
	setupRootExists:        "该目录已在列表中",
	setupRootLimit:         "最多配置 32 个代码目录",

	lspHeadline:        "语言服务配置（%s）",
	lspSubtitle:        "查看各语言服务在这台机器上的安装状态与版本，可按语言开关、自定义可执行路径及空闲回收时间。",
	lspReprobe:         "一键重新检测",
	lspReapBtn:         "停止运行中服务",
	lspReapedToast:     "已停止 %d 个运行中的语言服务",
	lspStatReady:       "已就绪语言",
	lspStatRunning:     "运行中实例",
	lspStatIdle:        "空闲回收",
	lspStatRepos:       "可用仓库",
	lspSectionList:     "语言探测状态（点按某行可编辑路径）",
	lspSectionEdit:     "配置 %s 语言服务",
	lspSectionIdle:     "空闲自动回收策略",
	lspBadgeReady:      "已就绪",
	lspBadgeMissing:    "未安装",
	lspBadgeDisabled:   "已禁用",
	lspBadgeBad:        "路径无效",
	lspRunningTag:      "运行中 %d",
	lspCustomTag:       "自定义路径",
	lspAutoTag:         "自动探测",
	lspNoInstallHint:   "请安装该语言对应的 LSP 服务，或手动指定可执行文件路径。",
	lspPathLabel:       "自定义 LSP 可执行路径（留空则自动在 PATH 中探测）",
	lspPathPlaceholder: "例如 ~/go/bin/gopls 或 /usr/local/bin/rust-analyzer",
	lspSaveBtn:         "保存并检测",
	lspResetBtn:        "恢复自动探测",
	lspEnableBtn:       "启用此语言",
	lspDisableBtn:      "禁用此语言",
	lspIdleLabel:       "空闲多久后自动关闭 LSP（分钟，1–1440）",
	lspIdleSaveBtn:     "保存回收策略",
	lspIdleUnit:        "%d 分钟",
	lspIdleInvalid:     "请输入 1 到 1440 之间的整数分钟数",
	lspSavedReady:      "%s 已就绪：%s",
	lspSavedBadPath:    "%s 路径不可用：%s",
	lspSavedDisabled:   "已禁用 %s 语言服务",
	lspSavedAuto:       "已恢复 %s 自动探测",
	lspIdleSaved:       "空闲回收时间已更新为 %d 分钟",
	lspConfigBtn:       "语言服务",
	lspNotReadyEmpty:   "还没有可用的机器",
	lspNotReadyHint:    "请先在配置向导里启用本机（Hub）或在扩展设置中选择电脑。",

	diffBtn:           "变更 Diff",
	diffCrumb:         "变更 Diff",
	diffBackList:      "← 变更列表",
	diffOpenSource:    "打开完整源码",
	diffModeUnstaged:  "未暂存",
	diffModeStaged:    "已暂存",
	diffModeBranch:    "对比主干",
	diffNotGit:        "当前仓库不是 Git 仓库",
	diffNotGitHint:    "未检测到 .git 目录，仍可正常浏览文件树与跳转源码。",
	diffNoBase:        "未找到默认主分支",
	diffNoBaseHint:    "当前仓库缺少 origin/HEAD、main 或 master 参考分支，请切换到「未暂存」或「已暂存」查看改动。",
	diffEmpty:         "当前范围内没有代码变更",
	diffEmptyHint:     "可以切换上方的「未暂存 / 已暂存 / 对比主干」查看其他范围的改动。",
	diffError:         "读取 Git 变更失败",
	diffTruncated:     "Diff 内容过大，已安全截断展示前部分内容；可点击「打开完整源码」直接阅读完整文件。",
	diffSummaryBranch: "分支：%s",
	diffSummaryBase:   "基准：%s",
	diffSummaryFiles:  "%d 个变更文件 · +%d -%d",
	diffBadgeAdded:    "新增",
	diffBadgeModified: "修改",
	diffBadgeDeleted:  "删除",
	diffBadgeRename:   "重命名",
	diffUntracked:     "未跟踪新文件",
	diffRenamedFrom:   "重命名自 %s",
	diffDeletedNoSrc:  "该文件已被删除，无法打开当前源码。",
	diffBinaryFile:    "二进制文件变更，不展示文本 Diff。",
	diffActionView:    "查看 Diff",
	diffActionJump:    "跳转源码",
	diffJumpTitle:     "点击修改位置直接跳转源码（高亮定位）",
	diffJumpLineFmt:   "L%d · %s",
	diffHunkBadgeAdd:  "新增/修改",
	diffHunkBadgeDel:  "删除位置",

	kinds: []string{"文件", "模块", "命名空间", "包", "类", "方法", "属性", "字段", "构造函数", "枚举", "接口", "函数", "变量", "常量",
		"字符串", "数字", "布尔", "数组", "对象", "键", "空", "枚举成员", "结构体", "事件", "运算符", "类型参数"},
	install: map[string]string{
		"go":         "Go：go install golang.org/x/tools/gopls@latest",
		"typescript": "TypeScript / JavaScript：npm install -g typescript-language-server typescript",
		"javascript": "TypeScript / JavaScript：npm install -g typescript-language-server typescript",
		"python":     "Python：npm install -g pyright，或 pip install python-lsp-server",
		"rust":       "Rust：rustup component add rust-analyzer",
		"swift":      "Swift：装上 Xcode 或 Swift 工具链（自带 sourcekit-lsp）",
		"c":          "C / C++：装上 clangd",
		"cpp":        "C / C++：装上 clangd",
	},
}

var en = text{
	title: "Code", machine: "Computer", searchHint: "Search symbols or text…", listSep: ", ",
	hubMachine: "Hub (local)",
	tabRepos:   "Repos", tabSetup: "Setup", tabLSP: "Languages",
	noMachines: "No computer chosen", noMachinesHint: "Enable Hub (local) in the setup guide below, or pick computers in extension settings and add your code folders.",
	noComputer: "Couldn't reach code navigation on this computer",
	repos:      "Repositories (%d)", repoList: "Repositories", noRepos: "No repositories found",
	noReposHint: "Open the Setup tab to add code folders and probe available repositories.", noReposIn: "No code repositories found in %s",
	lsp:         "Go to definition works for: %s",
	noLsp:       "No language server on this computer yet: you can read code and search text; open the Languages tab to probe or configure servers.",
	langMissing: "The %s code in this repository can be read but not followed yet. %s",
	files:       "Files (%d)", filesCut: ", partly listed", noFiles: "Couldn't list the files", outline: "Outline",
	noOutline: "No outline: ", noServer: "No %s language server on this computer", noServerHint: "No %s language server on this computer; install one to jump around. %s",
	back: "← Back", forward: "Forward →", backToCode: "Back to the code", pickFile: "Pick a file",
	pickFileHint: "Open a file from the tree or the folder above; tap a name in the outline to go there.",
	navHint:      "In the code, ⌘-click a name (or ⌘B) to go to its definition, ⌥F7 for usages, ⌘[ to go back.",
	noRead:       "Couldn't open this file", binary: "Binary file, not shown", earlier: "Show the %d lines above",
	outside: "Code outside the repository (a standard library or a dependency), read only.", fileCut: "The file is too long; the rest wasn't read.",
	findRefs: "Find usages", gotoDef: "Go to definition", gotoImpl: "Implementations", showHover: "Quick info",
	noSearch: "Couldn't search symbols", noMatch: "No symbol named “%s”", matches: "%d symbols match “%s”",
	symbolFallback:     "No language server works for this repository; these are text matches.",
	symbolFallbackLang: "No %s language server on this computer; these are text matches.",
	noRefs:             "Couldn't look it up", thisSymbol: "this symbol", refsOf: "Usages of “%s” (%d)", defsOf: "Definitions of “%s” (%d)",
	implsOf: "Implementations of “%s” (%d)", noneFound: "None found",
	noDef: "Couldn't go to the definition: ", noDefHere: "No definition found here.", noImplHere: "No implementation found here.",
	gotoLine: "Go to line", gotoLineHint: "Line, or line:column", gotoButton: "Go",
	lineNames: "Names on line %d", lineAt: "Line %d, column %d",
	lineNoServer: "Install a language server for this language to follow the names on this line.",
	hoverOf:      "About “%s”", hoverNone: "Nothing to show here.", noHover: "Couldn't show quick info", close: "Close",
	scopeSymbols: "Symbols", scopeText: "Text",
	grepTitle: "“%s” in %d files, %d places", grepNone: "“%s” not found", grepCut: "Only the first %d places are listed.",
	grepPartial: "Too many files: only %d were searched.", noGrep: "The text search failed", grepEmpty: "Type the text to find in the search box above",
	dirTitle: "%s (%d)", dirEmpty: "This folder is empty", folderWord: "Folder",
	symbolWord: "symbol",

	setupHeadline:          "Guided Setup",
	setupIntro:             "Pick which computers to browse (Hub local or paired nodes) and manage code folders. Strictly read-only: no repository file is ever modified.",
	setupStepMachines:      "Step 1 · Target computers",
	setupStepRoots:         "Step 2 · Code folders (%s)",
	setupStepNext:          "Step 3 · Verify & start browsing",
	setupHubDesc:           "Browse repositories directly on the Hub machine (read-only)",
	setupHubEnable:         "Enable Hub (local)",
	setupHubDisable:        "Disable Hub (local)",
	setupNodeEnabled:       "Authorized node · tap to manage its folders and language servers",
	setupNodePairedHint:    "Paired node; select it under Computers in extension settings to connect",
	setupOnline:            "Online",
	setupOffline:           "Offline",
	setupRootLabel:         "Add code folder",
	setupRootPlaceholder:   "e.g. ~/code or /path/to/workspace",
	setupRootAdd:           "Add & probe",
	setupRootEmpty:         "No code folders configured yet. Enter a path below or pick a detected folder.",
	setupRootRemove:        "Remove",
	setupRootRemoveConfirm: "Remove this folder from code navigation? Files on disk are not touched.",
	setupRootSuggestions:   "Detected code folders ready to add:",
	setupAddSuggestion:     "Add %s (%d repos)",
	setupRootOK:            "Readable · %d repositories found",
	setupRootMissing:       "Directory does not exist",
	setupRootPerm:          "Permission denied",
	setupRootForbidden:     "Protected or secret directory (blocked)",
	setupRootNotDir:        "Path is not a directory",
	setupRootReposCount:    "%d repos",
	setupBadgeEnabled:      "Enabled",
	setupBadgeDisabled:     "Off",
	setupBadgeOK:           "OK",
	setupBadgeMissing:      "Missing",
	setupBadgeDenied:       "Denied",
	setupBadgeBlocked:      "Blocked",
	setupStatMachines:      "Computers",
	setupStatRoots:         "Folders",
	setupStatRepos:         "Repositories",
	setupStatLSP:           "Ready LSPs",
	setupReprobe:           "Re-probe",
	setupToLSP:             "Configure languages",
	setupFinish:            "Finish & browse repos",
	setupOpenBtn:           "Open setup guide",
	setupNoTarget:          "Enable a computer first",
	setupNoTargetHint:      "Enable Hub (local) above, or choose remote nodes in extension settings.",
	setupSaved:             "Configuration saved and probed",
	setupRootInvalid:       "Invalid path: use an absolute path or start with ~/",
	setupRootExists:        "Folder is already in the list",
	setupRootLimit:         "At most 32 code folders are allowed",

	lspHeadline:        "Language Servers (%s)",
	lspSubtitle:        "Inspect detected LSP binaries and versions on this computer, toggle languages, set custom paths, and configure idle eviction.",
	lspReprobe:         "Probe now",
	lspReapBtn:         "Stop running servers",
	lspReapedToast:     "Stopped %d running language servers",
	lspStatReady:       "Ready",
	lspStatRunning:     "Running",
	lspStatIdle:        "Idle timeout",
	lspStatRepos:       "Repositories",
	lspSectionList:     "Language status (tap a row to configure its path)",
	lspSectionEdit:     "Configure %s",
	lspSectionIdle:     "Idle eviction policy",
	lspBadgeReady:      "Ready",
	lspBadgeMissing:    "Missing",
	lspBadgeDisabled:   "Disabled",
	lspBadgeBad:        "Invalid",
	lspRunningTag:      "%d running",
	lspCustomTag:       "Custom path",
	lspAutoTag:         "Auto-detect",
	lspNoInstallHint:   "Install a language server or enter a custom executable path.",
	lspPathLabel:       "Custom LSP executable path (leave blank for auto-detect)",
	lspPathPlaceholder: "e.g. ~/go/bin/gopls or /usr/local/bin/rust-analyzer",
	lspSaveBtn:         "Save & probe",
	lspResetBtn:        "Reset to auto",
	lspEnableBtn:       "Enable",
	lspDisableBtn:      "Disable",
	lspIdleLabel:       "Stop idle language servers after (minutes, 1–1440)",
	lspIdleSaveBtn:     "Save policy",
	lspIdleUnit:        "%d min",
	lspIdleInvalid:     "Enter a whole number between 1 and 1440",
	lspSavedReady:      "%s ready: %s",
	lspSavedBadPath:    "%s path unavailable: %s",
	lspSavedDisabled:   "Disabled %s language server",
	lspSavedAuto:       "Reset %s to auto-detect",
	lspIdleSaved:       "Idle timeout updated to %d minutes",
	lspConfigBtn:       "Languages",
	lspNotReadyEmpty:   "No computer enabled yet",
	lspNotReadyHint:    "Open the Setup tab to enable Hub (local) or choose computers in extension settings.",

	diffBtn:           "Diff",
	diffCrumb:         "Diff",
	diffBackList:      "← Changed files",
	diffOpenSource:    "Open full source",
	diffModeUnstaged:  "Unstaged",
	diffModeStaged:    "Staged",
	diffModeBranch:    "vs Main",
	diffNotGit:        "Not a Git repository",
	diffNotGitHint:    "No .git directory found; you can still browse files and jump to definitions.",
	diffNoBase:        "No default base branch found",
	diffNoBaseHint:    "Could not find origin/HEAD, main, or master; switch to Unstaged or Staged.",
	diffEmpty:         "No changes in this scope",
	diffEmptyHint:     "Switch between Unstaged, Staged, and vs Main above to check other changes.",
	diffError:         "Couldn't read Git diff",
	diffTruncated:     "Diff is large and was truncated for safety; use Open full source to read the whole file.",
	diffSummaryBranch: "Branch: %s",
	diffSummaryBase:   "Base: %s",
	diffSummaryFiles:  "%d changed files · +%d -%d",
	diffBadgeAdded:    "Added",
	diffBadgeModified: "Modified",
	diffBadgeDeleted:  "Deleted",
	diffBadgeRename:   "Renamed",
	diffUntracked:     "Untracked file",
	diffRenamedFrom:   "Renamed from %s",
	diffDeletedNoSrc:  "This file was deleted and cannot be opened in source view.",
	diffBinaryFile:    "Binary file change; text diff is omitted.",
	diffActionView:    "View diff",
	diffActionJump:    "Open source",
	diffJumpTitle:     "Jump to modified lines in source",
	diffJumpLineFmt:   "L%d · %s",
	diffHunkBadgeAdd:  "Changed",
	diffHunkBadgeDel:  "Deletion",

	kinds: []string{"file", "module", "namespace", "package", "class", "method", "property", "field", "constructor", "enum", "interface",
		"function", "variable", "constant", "string", "number", "boolean", "array", "object", "key", "null", "enum member", "struct",
		"event", "operator", "type parameter"},
	install: map[string]string{
		"go":         "Go: go install golang.org/x/tools/gopls@latest",
		"typescript": "TypeScript / JavaScript: npm install -g typescript-language-server typescript",
		"javascript": "TypeScript / JavaScript: npm install -g typescript-language-server typescript",
		"python":     "Python: npm install -g pyright, or pip install python-lsp-server",
		"rust":       "Rust: rustup component add rust-analyzer",
		"swift":      "Swift: install Xcode or a Swift toolchain (it comes with sourcekit-lsp)",
		"c":          "C / C++: install clangd",
		"cpp":        "C / C++: install clangd",
	},
}

// texts picks the page's language: Chinese for a zh locale or none, English otherwise.
func texts(locale string) text {
	if locale == "" || strings.HasPrefix(strings.ToLower(locale), "zh") {
		return zh
	}
	return en
}
