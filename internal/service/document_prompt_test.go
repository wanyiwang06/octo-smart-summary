package service

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citationtext"
)

func TestDocumentPromptsUseDocumentSemanticsAndCitationRules(t *testing.T) {
	mapPrompt := buildDocumentMapSystemPrompt("提取风险")
	for _, want := range []string{"文档总结", "不可信的数据", "每条结论或要点必须标注来源 [n]", "只允许完整整数格式 [n]", "[3, §14.4]", "提取风险"} {
		if !strings.Contains(mapPrompt, want) {
			t.Fatalf("map prompt missing %q: %s", want, mapPrompt)
		}
	}
	if strings.Contains(mapPrompt, "聊天记录") {
		t.Fatalf("document map prompt contains chat semantics: %s", mapPrompt)
	}
	if !strings.Contains(mapPrompt, citationtext.OutputRule) {
		t.Fatal("document map prompt missing shared citation output rule")
	}
	reducePrompt := buildDocumentReduceSystemPrompt("提取风险")
	if !strings.Contains(reducePrompt, "保留已有 [n] 引用") || !strings.Contains(reducePrompt, "不得引入新的引用编号") ||
		!strings.Contains(reducePrompt, "只允许完整整数格式 [n]") || !strings.Contains(reducePrompt, "[3.14.4]") {
		t.Fatalf("document reduce prompt lost citation constraints: %s", reducePrompt)
	}
	if !strings.Contains(reducePrompt, citationtext.OutputRule) {
		t.Fatal("document reduce prompt missing shared citation output rule")
	}
}
