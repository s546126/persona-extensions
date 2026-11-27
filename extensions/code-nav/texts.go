package main

import "strings"

// text is everything the page says, in one language.
type text struct {
	title, machine, searchHint, listSep                                  string
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
	kinds                                                                []string
	install                                                              map[string]string
}

// langTitle is how the page names a language.
var langTitle = map[string]string{
	"go": "Go", "typescript": "TypeScript", "javascript": "JavaScript", "python": "Python", "rust": "Rust",
	"swift": "Swift", "c": "C", "cpp": "C++",
}

func languageTitle(name string) string {
	if t, ok := langTitle[name]; ok {
		return t
	}
	return name
}

var zh = text{
	title: "代码", machine: "电脑", searchHint: "搜符号或文字…", listSep: "、",
	noMachines: "还没选电脑", noMachinesHint: "在扩展设置里选要看代码的电脑，再把代码所在的文件夹填进来（默认 ~/code）。",
	noComputer: "连不上这台电脑上的代码导航",
	repos:      "仓库（%d）", repoList: "仓库", noRepos: "没找到仓库",
	noReposHint: "在扩展设置里填代码所在的文件夹。", noReposIn: "这些文件夹里没找到代码仓库：%s",
	lsp:         "能跳转的语言：%s",
	noLsp:       "这台电脑上还没装语言服务，现在可以看代码和全文搜索；装上对应语言的服务就能跳转。",
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
	noMachines: "No computer chosen", noMachinesHint: "Choose the computers in the extension's settings, and add the folders your code is in (~/code by default).",
	noComputer: "Couldn't reach code navigation on this computer",
	repos:      "Repositories (%d)", repoList: "Repositories", noRepos: "No repositories found",
	noReposHint: "Add the folders your code is in to the extension's settings.", noReposIn: "No code repositories found in %s",
	lsp:         "Go to definition works for: %s",
	noLsp:       "No language server on this computer yet: you can read code and search text; install one for a language to jump around it.",
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
