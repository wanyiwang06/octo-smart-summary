package citationtext

import (
	"regexp"
	"strconv"
	"strings"
)

const documentArabicSectionNumber = `[0-9]+(?:[ \t]*\.[ \t]*[0-9]+)*(?:[ \t]*[-–—][ \t]*[0-9]+)?`
const documentChineseSectionNumber = `[〇零一二三四五六七八九十百千]+`
const documentSectionNumber = `(?:` + documentArabicSectionNumber + `|` + documentChineseSectionNumber + `)`
const documentSectionLocator = documentSectionNumber + `(?:[ \t]*\([A-Za-z0-9]+\))*`
const documentChineseSection = `(?:第[ \t]*` + documentSectionLocator + `[ \t]*(?:章|节|節|条|條|款|页|頁|段))+`
const documentEnglishSection = `(?i:(?:section|sec\.?|article|art\.?|page|p\.?|paragraph|para\.?))[ \t]*` + documentSectionLocator
const documentLabeledSection = `(?:§[ \t]*` + documentSectionLocator + `|` + documentChineseSection + `|` + documentEnglishSection + `)`
const documentSectionSeparator = `[,，、;；]`
const documentLabeledSectionList = documentLabeledSection + `(?:[ \t]*` + documentSectionSeparator + `[ \t]*` + documentLabeledSection + `)*`

var documentLabeledBracketCandidate = regexp.MustCompile(`\[[ \t]*(?:[0-9]{1,5}[ \t]*` + documentSectionSeparator + `[ \t]*)?` + documentLabeledSectionList + `[ \t]*\]`)
var documentLeadingIndex = regexp.MustCompile(`^([0-9]{1,5})(.*)$`)
var documentLabeledSectionTail = regexp.MustCompile(`^[ \t]*` + documentSectionSeparator + `[ \t]*` + documentLabeledSectionList + `[ \t]*$`)
var documentLabeledSectionOnly = regexp.MustCompile(`^[ \t]*` + documentLabeledSectionList + `[ \t]*$`)

// DocumentEvidenceForModel is the single document-provenance boundary for raw
// text entering a model prompt. It turns citation-looking source prose into
// ordinary parenthesized prose so the model cannot copy it as a summary
// citation. Callers must add formatter-owned evidence markers only AFTER this
// function runs.
//
// This deliberately also rewrites matches inside Markdown code, links, images,
// and escapes. Prompt safety is more important than preserving source Markdown
// syntax here: protecting those spans would reopen a path by which a model can
// repeat a source-owned bracket as a fabricated clickable citation.
func DocumentEvidenceForModel(content string) string {
	// Use the exact numeric candidate consumed by Scan. This is intentionally a
	// direct package-level dependency: the safety boundary must not maintain a
	// second, subtly narrower grammar. In particular, candidate can find the
	// inner marker in nested text such as [[1]] and has no body-length cap.
	content = replaceDocumentPromptBrackets(content, candidate, func(body string) bool {
		return !dateShape.MatchString(strings.TrimSpace(body))
	})
	return replaceDocumentPromptBrackets(content, documentLabeledBracketCandidate, func(string) bool {
		return true
	})
}

func replaceDocumentPromptBrackets(content string, pattern *regexp.Regexp, replace func(body string) bool) string {
	var b strings.Builder
	offset := 0
	changed := false
	for _, loc := range pattern.FindAllStringIndex(content, -1) {
		start, end := loc[0], loc[1]
		if !replace(content[start+1 : end-1]) {
			continue
		}
		b.WriteString(content[offset:start])
		b.WriteByte('(')
		b.WriteString(content[start+1 : end-1])
		b.WriteByte(')')
		offset = end
		changed = true
	}
	if !changed {
		return content
	}
	b.WriteString(content[offset:])
	return b.String()
}

// NormalizeDocumentSectionMarkers collapses document-section pseudo-citations
// invented by a model to the source ordinal the product can actually resolve.
// Examples: [3, §14.4] and [3, 第14节] become [3]. Bare dotted
// brackets such as [3.14.1] are intentionally preserved because they are
// indistinguishable from legitimate versions, decimal ranges, and IP addresses.
//
// This is deliberately document-only. Chat summaries and generic Markdown may
// legitimately contain bracketed dotted numbers, while document generation has
// an explicit [N]-only citation contract and a known source-index set.
func NormalizeDocumentSectionMarkers(content string, valid func(int) bool) string {
	protected := protectedSpans(content)
	markers := Scan(content)
	markerCursor := 0
	var b strings.Builder
	offset := 0
	changed := false
	for _, loc := range documentLabeledBracketCandidate.FindAllStringIndex(content, -1) {
		start, end := loc[0], loc[1]
		for markerCursor < len(markers) && markers[markerCursor].End <= start {
			markerCursor++
		}
		if protected[start] || escaped(content, start) ||
			(start > 0 && content[start-1] == '!') ||
			(end < len(content) && (content[end] == '(' || content[end] == ':')) {
			continue
		}
		if (start > 0 && content[start-1] == '[') || (end < len(content) && content[end] == ']') {
			// Folding an inner labeled marker would turn nested source prose such
			// as [[3, §14]] into the live citation [[3]]. Keep the section text
			// but remove its citation syntax instead.
			b.WriteString(content[offset:start])
			b.WriteByte('(')
			b.WriteString(content[start+1 : end-1])
			b.WriteByte(')')
			offset = end
			changed = true
			continue
		}
		body := strings.TrimSpace(content[start+1 : end-1])
		match := documentLeadingIndex.FindStringSubmatch(body)
		if match == nil {
			if documentLabeledSectionOnly.MatchString(body) {
				b.WriteString(content[offset:start])
				b.WriteByte('(')
				b.WriteString(content[start+1 : end-1])
				b.WriteByte(')')
				offset = end
				changed = true
			}
			continue
		}
		if !documentLabeledSectionTail.MatchString(match[2]) {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil || valid == nil || !valid(index) {
			continue
		}
		// Folding next to a compound bracket would make that bracket look like a
		// citation cluster to a later normalization pass. Preserve the labeled
		// marker so repeated save/refine normalization remains byte-stable.
		if adjacentToCompoundMarker(content, markers, markerCursor, start, end) {
			continue
		}
		b.WriteString(content[offset:start])
		b.WriteByte('[')
		b.WriteString(strconv.Itoa(index))
		b.WriteByte(']')
		offset = end
		changed = true
	}
	if !changed {
		return content
	}
	b.WriteString(content[offset:])
	return b.String()
}

func adjacentToCompoundMarker(content string, markers []Marker, cursor, start, end int) bool {
	// Markers and bracket candidates are both ordered. Only the nearest marker
	// on either side can be adjacent, so the caller's monotonic cursor keeps the
	// complete normalization pass O(brackets + markers), including large docs.
	if cursor > 0 {
		previous := markers[cursor-1]
		if previous.Compound && onlyHorizontalSpace(content, previous.End, start) {
			return true
		}
	}
	if cursor < len(markers) {
		next := markers[cursor]
		if next.Start >= end && next.Compound && onlyHorizontalSpace(content, end, next.Start) {
			return true
		}
	}
	return false
}
