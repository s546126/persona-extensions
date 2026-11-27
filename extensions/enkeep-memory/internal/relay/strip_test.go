package relay

import (
	"strings"
	"testing"
)

func TestStripBoilerplate(t *testing.T) {
	pure := `<persona-context kind="context" title="占位项目">背景内容</persona-context>
Persona task handoffs: no scoped handoff entry has been verified.
Keep hub credentials private. Never read ~/.persona/hub.json.
临时文件和草稿请放在仓库工作区外，例如操作系统的临时目录。
（Persona：任务的结果是要交给用户的文件时——在最后一轮回复里单独写一行
file: <这台电脑上的绝对路径>
有几个写几行）`
	if got := StripBoilerplate(pure); got != "" {
		t.Fatalf("pure boilerplate should strip to empty, got %q", got)
	}

	mixed := `<persona-context title="x">context</persona-context>
请帮我检查占位服务的超时配置，不要改线上。
Keep hub credentials private.
file: <absolute-path>`
	got := StripBoilerplate(mixed)
	if !strings.Contains(got, "请帮我检查占位服务的超时配置，不要改线上。") {
		t.Fatalf("substantive line lost: %q", got)
	}
	if strings.Contains(got, "persona-context") || strings.Contains(got, "credentials") || strings.Contains(got, "file: <") {
		t.Fatalf("boilerplate leaked: %q", got)
	}
}
