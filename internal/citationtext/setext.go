// Package citationtext — setext heading neutralization.
//
// LLM-generated summary content occasionally ends a lead-in paragraph with a
// horizontal-rule line, e.g.
//
//	根据提供的 45 条混合证据（含聊天记录与文档），现将项目进展整理如下：
//	---
//	### 一、已完成事项
//
// In CommonMark a line of dashes immediately following a non-blank line is the
// SETEXT H2 HEADING syntax, not a thematic break — the whole lead-in paragraph
// becomes an <h2>. Consumers that render headings with special layout (the web
// summary view numbers h2 elements and lays them out as flex containers) then
// split the paragraph's plain and bold runs into separate flex items, producing
// a broken multi-column heading. The model never intends a heading here: it
// means a visual separator.
//
// The fix is deterministic post-processing at every content write site: insert
// a blank line between the paragraph and the dash/equals rule so the renderer
// keeps them as paragraph + thematic break. This is intentionally conservative:
// only lines consisting solely of 3+ dashes or 3+ equals signs (plus surrounding
// whitespace) are treated, and lines inside fenced code blocks and indented
// code blocks are left byte-identical.
package citationtext

import "strings"

const (
	maxSetextContentRunes = 10000
	setextMaxRunes        = 200000
)

// NormalizeSetextHeadings returns content with a blank line inserted between a
// non-blank paragraph line and a following setext underline (--- or ===). Lines
// inside fenced (``` or ~~~) or indented (4-space/tab) code blocks are never
// modified. ATX headings (## …), list items, quotes and table rows are not
// underlines for this purpose, so their --- neighbours inside those constructs
// stay untouched except for the one ambiguity that matters: a bare rule after
// a list/table/quote line is still followed by setext rules in CommonMark only
// when the preceding line could close the container; we only normalize the
// plain-paragraph case, which is the shape the model actually emits.
func NormalizeSetextHeadings(content string) string {
	if !strings.Contains(content, "\n") {
		return content
	}
	if len(content) > setextMaxRunes {
		// Defensive bound: content is capped far below this at every write
		// site (500KB), so an oversized body is never modified rather than
		// scanned.
		return content
	}
	lines := strings.Split(content, "\n")
	// inFence tracks fenced code state; fenceMarker is the opening sequence
	// (at least 3 backticks or tildes). A closing fence must be at least as
	// long and use the same character.
	inFence := false
	fenceChar := byte(0)
	fenceLen := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if inFence {
			if closesFence(line, fenceChar, fenceLen) {
				inFence = false
			}
			continue
		}
		if open, ch, n := opensFence(line); open {
			inFence = true
			fenceChar = ch
			fenceLen = n
			continue
		}
		if isIndentedCode(line) {
			continue
		}
		if !isSetextUnderline(line) {
			continue
		}
		if i == 0 {
			continue
		}
		prev := lines[i-1]
		if prev == "" || isIndentedCode(prev) || isSetextUnderline(prev) {
			// A blank line, indented code, or a chain of rules before the
			// underline already breaks the setext interpretation; leave as-is.
			continue
		}
		if runeCount(prev) > maxSetextContentRunes {
			continue
		}
		// Insert a blank line before the underline: paragraph + thematic break.
		lines = append(lines[:i], append([]string{""}, lines[i:]...)...)
		i++ // skip the blank we just inserted
	}
	return strings.Join(lines, "\n")
}

// isSetextUnderline reports whether the line is exactly 3+ '-' or 3+ '='
// characters with optional leading/trailing spaces — the only shapes that act
// as setext underlines for a preceding paragraph line.
func isSetextUnderline(line string) bool {
	trimmed := strings.TrimRight(strings.TrimLeft(line, " "), " \t")
	if len(trimmed) < 3 {
		return false
	}
	if trimmed[0] != '-' && trimmed[0] != '=' {
		return false
	}
	c := trimmed[0]
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != c {
			return false
		}
	}
	return true
}

// opensFence detects a fenced code block opener: 3+ backticks or tildes,
// optionally indented up to 3 spaces, with an optional info string.
func opensFence(line string) (bool, byte, int) {
	trimmed := strings.TrimLeft(line, " ")
	if len(trimmed)-len(strings.TrimLeft(trimmed, " \t")) > 3 {
		return false, 0, 0
	}
	if len(trimmed) < 3 {
		return false, 0, 0
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return false, 0, 0
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == c {
		n++
	}
	if n < 3 {
		return false, 0, 0
	}
	// Backtick fences must not have backticks in the info string; tildes may.
	if c == '`' && strings.Contains(trimmed[n:], "`") {
		return false, 0, 0
	}
	return true, c, n
}

// closesFence reports whether line closes the current fence: same character,
// length at least the opener's, nothing but spaces after.
func closesFence(line string, c byte, n int) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(trimmed)-len(strings.TrimLeft(trimmed, " \t")) > 3 {
		return false
	}
	i := 0
	for i < len(trimmed) && trimmed[i] == c {
		i++
	}
	if i < n {
		return false
	}
	return strings.TrimSpace(trimmed[i:]) == ""
}

// isIndentedCode reports whether the line starts with 4 spaces or a tab
// (indented code block content in CommonMark, when not inside a list item —
// list continuation lines are rare in generated summaries and the
// conservative behavior of leaving them untouched is safe).
func isIndentedCode(line string) bool {
	return strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
}

func runeCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
