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

// B-1 (yujiawei §1 / mocha P1 / Jerry-Xin B-4, round-1 PR#268): '=' is NOT a
// thematic-break character in CommonMark, so neutralizing a '===' underline
// surfaces it as literal visible text and destroys a correctly-rendering H1.
// The dash rule stays normalizable; equals underlines must stay byte-identical.
func TestNormalizeSetextHeadingsEqualsUnderlineUntouched(t *testing.T) {
	in := "总结标题行\n===\n后续内容\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("equals underline must stay byte-identical:\n got=%q\nwant=%q", got, in)
	}
}

// B-3 over-fire leg (yujiawei 2.2 / Jerry-Xin B-3): a 4-space-indented closing
// fence is code CONTENT in CommonMark, so the block stays open and the trailing
// '---' after 'text' must NOT get a blank line inserted inside the code block.
func TestNormalizeSetextHeadingsNoBlankInsideOpenCodeBlock(t *testing.T) {
	in := "```\ncode\n    ```\ntext\n---\nmore\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("4-space-indented closer is code content; blank line must not be inserted:\n got=%q\nwant=%q", got, in)
	}
}

// B-3 under-fire leg: a 4-space-indented fence-lookalike is indented code, not
// a fence. opensFence must not flip fence state on it, so later real setext
// shapes are still normalized.
func TestNormalizeSetextHeadingsIndentedCodeBeforeFenceLookalike(t *testing.T) {
	in := "para\n\n    ```text\n段落B\n---\n尾部\n"
	want := "para\n\n    ```text\n段落B\n\n---\n尾部\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("indented fence-lookalike must not suppress later normalization:\n got=%q\nwant=%q", got, want)
	}
}

// P2-2 (yujiawei 2.5 / Jerry-Xin P2-2): CRLF content must normalize; '\r'
// left by the '\n' split must not defeat the underline/blank-line checks.
func TestNormalizeSetextHeadingsCRLF(t *testing.T) {
	in := "段落文字\r\n---\r\n### 后续\r\n"
	want := "段落文字\r\n\r\n---\r\n### 后续\r\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("CRLF input must be normalized:\n got=%q\nwant=%q", got, want)
	}
}

// P2-1 (yujiawei 2.4 / mocha / Jerry-Xin P2-1): write sites accept up to
// maxContentBytes (500KB); the neutralizer's own bound must cover that range.
// 135KB of valid body must not bypass the fix.
func TestNormalizeSetextHeadingsLargeBodyWithinContentCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("# 报告\n\n")
	// 300KB: above the old 200000-byte guard, below maxContentBytes (512000)
	// that every write site accepts — exactly the bypass band the reviewers
	// flagged (yujiawei 2.4 executed probe shape).
	for b.Len() < 300*1024 {
		b.WriteString("这是一段足够长的正文内容，用于把文档体量推过旧的 200KB 字节守卫。\n\n")
	}
	b.WriteString("结论段落\n---\n后续\n")
	in := b.String()
	want := strings.Replace(in, "结论段落\n---", "结论段落\n\n---", 1)
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("body within maxContentBytes must still be normalized (got len=%d)", len(got))
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
