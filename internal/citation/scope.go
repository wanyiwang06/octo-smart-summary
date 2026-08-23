// Package citation holds the vocabulary shared by every stage that reads or
// rewrites `[n]` citation markers inside a generated body.
//
// It exists because the repo has now had to answer the SAME question in three
// places — "is this bracketed token a citation marker, or is it ordinary
// content?" — and answering it twice is how the R11 Q5 defect happened: a
// strip that deleted every bracketed integer anywhere, including `items[0]`
// inside fenced code, `GB/T 7714 [2020]`, and the `[1]` out of `[1](url)`.
//
// Import direction: internal/api/handler already imports internal/worker
// (agent_summary_citations.go), so the worker cannot import the handler and the
// hardened helpers in handler/share.go cannot be reused upward. A leaf package
// depending on nothing but the standard library is the only shape that lets
// BOTH call sites share one definition instead of growing a second one.
package citation

import "strings"

// MarkerRewriter decides what happens to one bracketed token that has already
// passed the syntactic scoping rules below.
//
// token is the raw text between the brackets ("3", "P2", "2020", "+5"): the
// callers disagree about what a marker looks like (the handler matches an
// explicit `[n]`/`[Pn]` marker set, the worker parses a 1-based pool ordinal),
// so the SYNTAX is shared here and the SEMANTICS stay with the caller.
//
// Returning rewrite=false leaves the token byte-identical. That is the default
// for anything the caller cannot positively identify: an unrecognised `[2020]`
// in prose is correct content, and deleting it is data loss, whereas keeping it
// costs nothing.
type MarkerRewriter func(token string) (replacement string, rewrite bool)

// RewriteMarkers walks content and offers every syntactically-eligible
// bracketed token to fn, splicing in whatever fn asks for.
//
// Scoping rules — all of them NARROWING, all of them settled previously in
// handler.stripUnresolvedCitationMarkers (R11 Q5) and re-derived here once:
//
//   - CommonMark fenced code regions (backtick or tilde, with the closing run
//     at least as long as the opening run) are passed through untouched;
//   - matched inline-code spans are passed through untouched, including spans
//     that cross a line break;
//   - a markdown inline link `[1](url)` is content, never a marker — deleting
//     or renumbering the `[1]` silently corrupts the link;
//   - a named reference-style link `[1][docs]` is content for the same reason;
//   - `[1][2]`, `[P1][P2]` and mixed citation-shaped pairs are treated as
//     adjacent markers unless the second label is really defined elsewhere in
//     the document;
//   - a reference definition `[1]: https://…` is exempt only at line start
//     (with up to three leading spaces), not in prose such as `根据 [1]: ...`;
//   - an unterminated `[` is copied verbatim, but does not hide a complete
//     marker immediately before or after it.
//
// Everything else is offered to fn, which may still decline it.
//
// Whitespace around a removal is deliberately NOT touched here: the handler
// pins the surrounding spacing byte-for-byte, so tidying belongs to the caller
// that wants it.
func RewriteMarkers(content string, fn MarkerRewriter) string {
	if fn == nil {
		return content
	}
	segments := splitFencedSegments(content)
	definitions := collectReferenceDefinitions(segments)
	var b strings.Builder
	b.Grow(len(content))
	for _, segment := range segments {
		if segment.protected {
			b.WriteString(segment.text)
			continue
		}
		b.WriteString(rewriteMarkersInText(segment.text, definitions, fn))
	}
	return b.String()
}

type markdownSegment struct {
	text      string
	protected bool
}

type fenceState struct {
	marker byte
	length int
}

// splitFencedSegments applies CommonMark's core fence rules: an opener has up
// to three leading spaces and at least three matching backticks or tildes; a
// closer uses the same character, a run at least as long as the opener, and no
// trailing non-whitespace. Keeping the opening length prevents a literal ```
// line inside a ```` block from ending the block early.
func splitFencedSegments(content string) []markdownSegment {
	if content == "" {
		return nil
	}
	segments := make([]markdownSegment, 0, 3)
	appendSegment := func(text string, protected bool) {
		if text == "" {
			return
		}
		if len(segments) > 0 && segments[len(segments)-1].protected == protected {
			segments[len(segments)-1].text += text
			return
		}
		segments = append(segments, markdownSegment{text: text, protected: protected})
	}

	var fence fenceState
	for start := 0; start < len(content); {
		next := strings.IndexByte(content[start:], '\n')
		if next < 0 {
			next = len(content)
		} else {
			next += start + 1
		}
		line := content[start:next]
		bare := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if fence.length > 0 {
			appendSegment(line, true)
			if closesFence(bare, fence) {
				fence = fenceState{}
			}
		} else if opened, ok := opensFence(bare); ok {
			appendSegment(line, true)
			fence = opened
		} else {
			appendSegment(line, false)
		}
		start = next
	}
	return segments
}

func opensFence(line string) (fenceState, bool) {
	i := 0
	for i < len(line) && i < 4 && line[i] == ' ' {
		i++
	}
	if i > 3 || i >= len(line) || (line[i] != '`' && line[i] != '~') {
		return fenceState{}, false
	}
	marker := line[i]
	j := i
	for j < len(line) && line[j] == marker {
		j++
	}
	if j-i < 3 {
		return fenceState{}, false
	}
	// A backtick fence's info string cannot itself contain a backtick.
	if marker == '`' && strings.ContainsRune(line[j:], '`') {
		return fenceState{}, false
	}
	return fenceState{marker: marker, length: j - i}, true
}

func closesFence(line string, fence fenceState) bool {
	i := 0
	for i < len(line) && i < 4 && line[i] == ' ' {
		i++
	}
	if i > 3 || i >= len(line) || line[i] != fence.marker {
		return false
	}
	j := i
	for j < len(line) && line[j] == fence.marker {
		j++
	}
	return j-i >= fence.length && strings.Trim(line[j:], " \t") == ""
}

type textSpan struct {
	start int
	end   int
}

// inlineCodeSpans returns matched CommonMark backtick spans. An unmatched run
// remains prose, so it cannot suppress a real citation marker later in the
// document. Matching uses an equal-length closing run and may cross newlines.
func inlineCodeSpans(content string) []textSpan {
	var spans []textSpan
	for i := 0; i < len(content); {
		if content[i] != '`' || isBackslashEscaped(content, i) {
			i++
			continue
		}
		run := backtickRunLength(content, i)
		closeAt := -1
		for j := i + run; j < len(content); {
			if content[j] != '`' {
				j++
				continue
			}
			candidateRun := backtickRunLength(content, j)
			if candidateRun == run {
				closeAt = j
				break
			}
			j += candidateRun
		}
		if closeAt < 0 {
			i += run
			continue
		}
		spans = append(spans, textSpan{start: i, end: closeAt + run})
		i = closeAt + run
	}
	return spans
}

func backtickRunLength(content string, start int) int {
	end := start
	for end < len(content) && content[end] == '`' {
		end++
	}
	return end - start
}

func isBackslashEscaped(content string, at int) bool {
	backslashes := 0
	for i := at - 1; i >= 0 && content[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func spanContains(spans []textSpan, at int) bool {
	for _, span := range spans {
		if at < span.start {
			return false
		}
		if at < span.end {
			return true
		}
	}
	return false
}

// rewriteMarkersInText applies RewriteMarkers' rules to one non-fenced block.
// Inline code spans are protected before bracket scanning.
func rewriteMarkersInText(content string, definitions map[string]struct{}, fn MarkerRewriter) string {
	codeSpans := inlineCodeSpans(content)
	spanIndex := 0
	var b strings.Builder
	lineStart := 0
	for i := 0; i < len(content); {
		for spanIndex < len(codeSpans) && codeSpans[spanIndex].end <= i {
			spanIndex++
		}
		if spanIndex < len(codeSpans) && codeSpans[spanIndex].start <= i && i < codeSpans[spanIndex].end {
			span := codeSpans[spanIndex]
			b.WriteString(content[i:span.end])
			if lastNewline := strings.LastIndexByte(content[i:span.end], '\n'); lastNewline >= 0 {
				lineStart = i + lastNewline + 1
			}
			i = span.end
			spanIndex++
			continue
		}
		if content[i] != '[' {
			b.WriteByte(content[i])
			if content[i] == '\n' {
				lineStart = i + 1
			}
			i++
			continue
		}

		lineEnd := strings.IndexByte(content[i:], '\n')
		if lineEnd < 0 {
			lineEnd = len(content)
		} else {
			lineEnd += i
		}
		endOffset := strings.IndexByte(content[i:lineEnd], ']')
		if endOffset < 0 {
			// Copy only this unmatched opener. Advancing one byte instead of
			// consuming the rest of the line lets a later complete marker be
			// discovered independently.
			b.WriteByte(content[i])
			i++
			continue
		}
		end := i + endOffset
		// Likewise, a prose opener must not reach across an inline-code span to
		// steal a closing bracket from code.
		if spanIndex < len(codeSpans) && codeSpans[spanIndex].start < end {
			b.WriteByte(content[i])
			i++
			continue
		}
		// A stray opener before a real marker must not claim that marker's
		// closing bracket. Preserve the outer text and restart at the innermost
		// opener, which owns the first closing bracket.
		if nested := strings.LastIndexByte(content[i+1:end], '['); nested >= 0 {
			nested += i + 1
			b.WriteString(content[i:nested])
			i = nested
			continue
		}
		// A link or reference definition is content, not a marker.
		if end+1 < lineEnd {
			switch content[end+1] {
			case '(':
				b.WriteString(content[i : end+1])
				i = end + 1
				continue
			case ':':
				// `[label]: destination` is a reference definition only at
				// line start (CommonMark permits up to three leading spaces).
				// In prose such as `根据 [1]: 结论`, [1] is still a citation
				// marker and must be offered to the caller.
				line := content[lineStart:lineEnd]
				definitionLabel, isDefinition := referenceDefinitionLabel(line)
				if isDefinition && isReferenceDefinitionAt(line, i-lineStart) &&
					normalizeReferenceLabel(definitionLabel) == normalizeReferenceLabel(content[i+1:end]) {
					b.WriteString(content[i : end+1])
					i = end + 1
					continue
				}
			case '[':
				// A named reference link such as [1][docs] is content. A
				// citation-shaped second label (numeric or P+numeric), however,
				// is also the repository's adjacent-citation shape ([1][2],
				// [P1][P2]). Treat it as a link only when the document actually
				// defines that label; otherwise both tokens are offered to fn.
				if labelEnd := strings.IndexByte(content[end+1:lineEnd], ']'); labelEnd >= 0 {
					labelEnd += end + 1
					label := content[end+2 : labelEnd]
					_, definedReference := definitions[normalizeReferenceLabel(label)]
					if !isCitationLikeLabel(label) || definedReference {
						b.WriteString(content[i : labelEnd+1])
						i = labelEnd + 1
						continue
					}
				}
				// An unterminated second label is not enough to hide the first
				// marker. Process it normally; the unmatched tail is copied on
				// the next loop iteration.
			}
		}
		if replacement, rewrite := fn(content[i+1 : end]); rewrite {
			b.WriteString(replacement)
			i = end + 1
			continue
		}
		b.WriteString(content[i : end+1])
		i = end + 1
	}
	return b.String()
}

func collectReferenceDefinitions(segments []markdownSegment) map[string]struct{} {
	definitions := make(map[string]struct{})
	for _, segment := range segments {
		if segment.protected {
			continue
		}
		codeSpans := inlineCodeSpans(segment.text)
		for start := 0; start < len(segment.text); {
			next := strings.IndexByte(segment.text[start:], '\n')
			if next < 0 {
				next = len(segment.text)
			} else {
				next += start + 1
			}
			line := strings.TrimSuffix(strings.TrimSuffix(segment.text[start:next], "\n"), "\r")
			if label, markerStart, ok := referenceDefinition(line); ok && !spanContains(codeSpans, start+markerStart) {
				definitions[normalizeReferenceLabel(label)] = struct{}{}
			}
			start = next
		}
	}
	return definitions
}

func referenceDefinitionLabel(line string) (string, bool) {
	label, _, ok := referenceDefinition(line)
	return label, ok
}

func referenceDefinition(line string) (string, int, bool) {
	start := 0
	for start < len(line) && start < 3 && line[start] == ' ' {
		start++
	}
	if start >= len(line) || line[start] != '[' {
		return "", 0, false
	}
	end := strings.IndexByte(line[start:], ']')
	if end < 0 {
		return "", 0, false
	}
	end += start
	if end+1 >= len(line) || line[end+1] != ':' || end == start+1 {
		return "", 0, false
	}
	// Keep the recognizer intentionally single-line and conservative. An empty
	// `[label]:` is not a usable definition and must not suppress adjacent
	// numeric citation markers elsewhere in the document.
	if strings.TrimSpace(line[end+2:]) == "" {
		return "", 0, false
	}
	return line[start+1 : end], start, true
}

func isReferenceDefinitionAt(line string, markerStart int) bool {
	if markerStart > 3 {
		return false
	}
	for i := 0; i < markerStart; i++ {
		if line[i] != ' ' {
			return false
		}
	}
	return true
}

func isBareDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isCitationLikeLabel(s string) bool {
	return isBareDecimal(s) || (len(s) > 1 && s[0] == 'P' && isBareDecimal(s[1:]))
}

func normalizeReferenceLabel(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
