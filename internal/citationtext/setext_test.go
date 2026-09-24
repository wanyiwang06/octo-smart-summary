package citationtext

import (
	"strings"
	"testing"
)

func TestNormalizeSetextHeadingsInsertsBlankBeforeRule(t *testing.T) {
	in := "根据提供的 45 条混合证据（含聊天记录与文档），现将项目当前进展按 **已完成、进行中、风险阻塞、下一步计划** 四类整理如下：\n---\n### 一、已完成事项\n"
	want := "根据提供的 45 条混合证据（含聊天记录与文档），现将项目当前进展按 **已完成、进行中、风险阻塞、下一步计划** 四类整理如下：\n\n---\n### 一、已完成事项\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("NormalizeSetextHeadings mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeSetextHeadingsEqualsUnderline(t *testing.T) {
	in := "总结标题行\n===\n后续内容\n"
	want := "总结标题行\n\n===\n后续内容\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeSetextHeadingsLeavesStandaloneRule(t *testing.T) {
	// Blank line before --- already breaks setext; must stay byte-identical.
	in := "段落一\n\n---\n\n段落二\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("expected byte-identical output, got %q", got)
	}
}

func TestNormalizeSetextHeadingsLeavesFirstLineUnderline(t *testing.T) {
	in := "---\n内容\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("expected byte-identical output, got %q", got)
	}
}

func TestNormalizeSetextHeadingsSkipsFencedCode(t *testing.T) {
	in := "```\ntext\n---\nmore\n```\n段落\n---\n"
	want := "```\ntext\n---\nmore\n```\n段落\n\n---\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeSetextHeadingsSkipsTildeFence(t *testing.T) {
	in := "~~~go\ncode\n---\n~~~\n段落\n---\n"
	want := "~~~go\ncode\n---\n~~~\n段落\n\n---\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeSetextHeadingsSkipsIndentedCode(t *testing.T) {
	in := "段落:\n\n    indented\n    ---\n\n后续\n---\n"
	want := "段落:\n\n    indented\n    ---\n\n后续\n\n---\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeSetextHeadingsMixedDashesNotUnderline(t *testing.T) {
	// "- 4 -" is not a bare rule; must not be treated as underline.
	in := "标题文字\n- 4 -\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("expected byte-identical output, got %q", got)
	}
}

func TestNormalizeSetextHeadingsMultipleRules(t *testing.T) {
	in := "甲\n---\n乙\n---\n丙\n"
	want := "甲\n\n---\n乙\n\n---\n丙\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeSetextHeadingsNoNewline(t *testing.T) {
	in := "single line no rule"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("expected byte-identical output, got %q", got)
	}
}

func TestNormalizeSetextHeadingsOversized(t *testing.T) {
	var b strings.Builder
	b.Grow(setextMaxRunes + 10)
	for i := 0; i < setextMaxRunes+10; i++ {
		b.WriteByte('x')
	}
	b.WriteString("\n---\n")
	in := b.String()
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("oversized input must stay byte-identical")
	}
}
