package citationtext

import (
	"regexp"
	"strconv"
	"strings"
)

const documentSectionNumber = `[0-9]+(?:[ \t]*\.[ \t]*[0-9]+)*(?:[ \t]*[-–—][ \t]*[0-9]+)?`
const documentSectionLocator = documentSectionNumber + `(?:[ \t]*\([A-Za-z0-9]+\))*`
const documentChineseSection = `(?:第[ \t]*` + documentSectionLocator + `[ \t]*(?:章|节|節|条|條|款|页|頁|段))+`
const documentEnglishSection = `(?i:(?:section|sec\.?|article|art\.?|page|p\.?|paragraph|para\.?))[ \t]*` + documentSectionLocator
const documentLabeledSection = `(?:§[ \t]*` + documentSectionLocator + `|` + documentChineseSection + `|` + documentEnglishSection + `)`

var documentBracketCandidate = regexp.MustCompile(`\[[^\]\n]{1,256}\]`)
var documentLeadingIndex = regexp.MustCompile(`^([0-9]{1,5})(.*)$`)
var documentLabeledSectionTail = regexp.MustCompile(`^[ \t]*[,，][ \t]*` + documentLabeledSection + `(?:[ \t]*[,，][ \t]*` + documentLabeledSection + `)*[ \t]*$`)

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
	var b strings.Builder
	offset := 0
	changed := false
	for _, loc := range documentBracketCandidate.FindAllStringIndex(content, -1) {
		start, end := loc[0], loc[1]
		if protected[start] || escaped(content, start) ||
			(start > 0 && content[start-1] == '!') ||
			(end < len(content) && (content[end] == '(' || content[end] == ':')) {
			continue
		}
		body := strings.TrimSpace(content[start+1 : end-1])
		match := documentLeadingIndex.FindStringSubmatch(body)
		if match == nil || !documentLabeledSectionTail.MatchString(match[2]) {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil || valid == nil || !valid(index) {
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
