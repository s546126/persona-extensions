package main

import "strings"

// text is everything the page says, in one language.
type text struct {
	title, machine, searchHint, listSep                             string
	noMachines, noMachinesHint, noComputer                          string
	repos, repoList, noRepos, noReposHint, noReposIn                string
	lsp, noLsp, noGopls                                             string
	files, filesCut, noFiles, outline, noOutline, noServer          string
	back, backToCode, pickFile, pickFileHint                        string
	noRead, binary, earlier, outside, fileCut                       string
	findRefs, noSearch, noMatch, matches                            string
	noRefs, thisSymbol, refsOf, defsOf, noneFound, noDef, noDefHere string
	symbolWord                                                      string
	kinds                                                           []string
}

var zh = text{
	title: "代码", machine: "电脑", searchHint: "搜符号（函数、类型…）", listSep: "、",
	noMachines: "还没选电脑", noMachinesHint: "在扩展设置里选要看代码的电脑，再把代码所在的文件夹填进来（默认 ~/code）。",
	noComputer: "连不上这台电脑上的代码导航",
	repos:      "仓库（%d）", repoList: "仓库", noRepos: "没找到仓库",
	noReposHint: "在扩展设置里填代码所在的文件夹。", noReposIn: "这些文件夹里没有 Git 仓库：%s",
	lsp: "能跳转的语言：%s", noLsp: "这台电脑上没找到语言服务，只能看代码。装 Go 的：go install golang.org/x/tools/gopls@latest",
	noGopls: "。Go 要跳转需要装 gopls：go install golang.org/x/tools/gopls@latest",
	files:   "文件（%d）", filesCut: "，只列了一部分", noFiles: "列不出文件", outline: "大纲",
	noOutline: "大纲出不来：", noServer: "这台电脑上没有 %s 的语言服务",
	back: "← 后退", backToCode: "回到代码", pickFile: "选个文件看看", pickFileHint: "在左边的文件树里点开文件；在代码里按 ⌘B 跳到定义。",
	noRead: "打不开这个文件", binary: "这是二进制文件，不显示", earlier: "显示前面的 %d 行",
	outside: "这是仓库外的代码（Go 标准库或依赖），只读。", fileCut: "文件太长，后面没读进来。",
	findRefs: "查找引用", noSearch: "搜不了符号", noMatch: "没有叫「%s」的符号", matches: "%d 个符号匹配「%s」",
	noRefs: "查不了", thisSymbol: "这个符号", refsOf: "「%s」的引用（%d）", defsOf: "「%s」的定义（%d）",
	noneFound: "一个也没找到", noDef: "没能跳到定义：", noDefHere: "这里没找到定义。",
	symbolWord: "符号",
	kinds: []string{"文件", "模块", "命名空间", "包", "类", "方法", "属性", "字段", "构造函数", "枚举", "接口", "函数", "变量", "常量",
		"字符串", "数字", "布尔", "数组", "对象", "键", "空", "枚举成员", "结构体", "事件", "运算符", "类型参数"},
}

var en = text{
	title: "Code", machine: "Computer", searchHint: "Search symbols (functions, types…)", listSep: ", ",
	noMachines: "No computer chosen", noMachinesHint: "Choose the computers in the extension's settings, and add the folders your code is in (~/code by default).",
	noComputer: "Couldn't reach code navigation on this computer",
	repos:      "Repositories (%d)", repoList: "Repositories", noRepos: "No repositories found",
	noReposHint: "Add the folders your code is in to the extension's settings.", noReposIn: "No Git repositories in %s",
	lsp: "Go to definition works for: %s", noLsp: "No language server on this computer, so code can only be read. For Go: go install golang.org/x/tools/gopls@latest",
	noGopls: ". For Go, install gopls: go install golang.org/x/tools/gopls@latest",
	files:   "Files (%d)", filesCut: ", partly listed", noFiles: "Couldn't list the files", outline: "Outline",
	noOutline: "No outline: ", noServer: "No %s language server on this computer",
	back: "← Back", backToCode: "Back to the code", pickFile: "Pick a file", pickFileHint: "Open a file in the tree; in the code, ⌘B goes to a definition.",
	noRead: "Couldn't open this file", binary: "Binary file, not shown", earlier: "Show the %d lines above",
	outside: "Code outside the repository (Go's standard library or a dependency), read only.", fileCut: "The file is too long; the rest wasn't read.",
	findRefs: "Find usages", noSearch: "Couldn't search symbols", noMatch: "No symbol named “%s”", matches: "%d symbols match “%s”",
	noRefs: "Couldn't look it up", thisSymbol: "this symbol", refsOf: "Usages of “%s” (%d)", defsOf: "Definitions of “%s” (%d)",
	noneFound: "None found", noDef: "Couldn't go to the definition: ", noDefHere: "No definition found here.",
	symbolWord: "symbol",
	kinds: []string{"file", "module", "namespace", "package", "class", "method", "property", "field", "constructor", "enum", "interface",
		"function", "variable", "constant", "string", "number", "boolean", "array", "object", "key", "null", "enum member", "struct",
		"event", "operator", "type parameter"},
}

// texts picks the page's language: Chinese for a zh locale or none, English otherwise.
func texts(locale string) text {
	if locale == "" || strings.HasPrefix(strings.ToLower(locale), "zh") {
		return zh
	}
	return en
}
