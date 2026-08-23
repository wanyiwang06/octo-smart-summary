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
//   - fenced code regions (``` ... ```) are passed through untouched, so
//     `items[0] = x` in a Go block survives;
//   - a markdown inline link `[1](url)` is content, never a marker — deleting
//     or renumbering the `[1]` silently corrupts the link;
//   - a named reference-style link `[1][docs]` is content for the same reason;
//   - `[1][2]` is treated as two adjacent markers unless `[2]: ...` is really
//     defined elsewhere in the document;
//   - a reference definition `[1]: https://…` is exempt only at line start
//     (with up to three leading spaces), not in prose such as `根据 [1]: ...`;
//   - an unterminated `[` is copied verbatim, but does not hide a complete
//     marker immediately before it.
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
	lines := strings.Split(content, "\n")
	definitions := collectReferenceDefinitions(lines)
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			lines[i] = rewriteMarkersInLine(line, definitions, fn)
		}
	}
	return strings.Join(lines, "\n")
}

// rewriteMarkersInLine applies RewriteMarkers' rules to a single non-fenced
// line. See RewriteMarkers for the scoping rules.
func rewriteMarkersInLine(line string, definitions map[string]struct{}, fn MarkerRewriter) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '[' {
			b.WriteByte(line[i])
			i++
			continue
		}
		end := strings.IndexByte(line[i:], ']')
		if end < 0 {
			// Unterminated bracket: emit the rest verbatim rather than
			// consuming it.
			b.WriteString(line[i:])
			break
		}
		end += i
		// A link or reference definition is content, not a marker.
		if end+1 < len(line) {
			switch line[end+1] {
			case '(':
				b.WriteString(line[i : end+1])
				i = end + 1
				continue
			case ':':
				// `[label]: destination` is a reference definition only at
				// line start (CommonMark permits up to three leading spaces).
				// In prose such as `根据 [1]: 结论`, [1] is still a citation
				// marker and must be offered to the caller.
				definitionLabel, isDefinition := referenceDefinitionLabel(line)
				if isDefinition && isReferenceDefinitionAt(line, i) &&
					normalizeReferenceLabel(definitionLabel) == normalizeReferenceLabel(line[i+1:end]) {
					b.WriteString(line[i : end+1])
					i = end + 1
					continue
				}
			case '[':
				// A named reference link such as [1][docs] is content. A bare
				// numeric second label, however, is also the repository's real
				// adjacent-citation shape ([1][2]). Treat it as a link only when
				// the document actually defines that numeric label; otherwise
				// both bracketed numbers are offered independently to fn.
				if labelEnd := strings.IndexByte(line[end+1:], ']'); labelEnd >= 0 {
					labelEnd += end + 1
					label := line[end+2 : labelEnd]
					_, numericDefinition := definitions[normalizeReferenceLabel(label)]
					if !isBareDecimal(label) || numericDefinition {
						b.WriteString(line[i : labelEnd+1])
						i = labelEnd + 1
						continue
					}
				}
				// An unterminated second label is not enough to hide the first
				// marker. Process it normally; the unmatched tail is copied on
				// the next loop iteration.
			}
		}
		if replacement, rewrite := fn(line[i+1 : end]); rewrite {
			b.WriteString(replacement)
			i = end + 1
			continue
		}
		b.WriteString(line[i : end+1])
		i = end + 1
	}
	return b.String()
}

func collectReferenceDefinitions(lines []string) map[string]struct{} {
	definitions := make(map[string]struct{})
	inFence := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if label, ok := referenceDefinitionLabel(line); ok {
			definitions[normalizeReferenceLabel(label)] = struct{}{}
		}
	}
	return definitions
}

func referenceDefinitionLabel(line string) (string, bool) {
	start := 0
	for start < len(line) && start < 3 && line[start] == ' ' {
		start++
	}
	if start >= len(line) || line[start] != '[' {
		return "", false
	}
	end := strings.IndexByte(line[start:], ']')
	if end < 0 {
		return "", false
	}
	end += start
	if end+1 >= len(line) || line[end+1] != ':' || end == start+1 {
		return "", false
	}
	// Keep the recognizer intentionally single-line and conservative. An empty
	// `[label]:` is not a usable definition and must not suppress adjacent
	// numeric citation markers elsewhere in the document.
	if strings.TrimSpace(line[end+2:]) == "" {
		return "", false
	}
	return line[start+1 : end], true
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

func normalizeReferenceLabel(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
