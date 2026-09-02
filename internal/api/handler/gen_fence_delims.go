//go:build ignore

// Command gen_fence_delims derives the fence delimiter fold table from Unicode
// character properties and writes fence_delims_table.go.
//
// Run with: go generate ./internal/api/handler
//
// Rationale: the hand-written homoglyph list in normalizeRefFenceSyntax folded
// exactly three runes (＜ ＞ ／). A hand-written list is a set the attacker picks
// from the complement, and the complement here is large: ⟨ ⟩ (U+27E8/9), 〈 〉
// (U+2329/A) and ﹤ ﹥ (U+FE64/5) all render as bare angle brackets and all
// reached the model byte-identical. Deriving the list from Unicode properties
// means new members are found by re-running this, not by waiting for someone to
// guess one.
//
// Two derivation rules for the DELIMITER alphabet, both mechanical:
//
//	A. NFKC(r) is exactly "<", ">" or "/" — compatibility equivalence.
//	B. The Unicode NAME marks r as an angle-bracket or solidus homoglyph, with
//	   composite/decorated forms excluded (they do not read as a bare delimiter),
//	   and REVERSE SOLIDUS excluded (a distinct character, not a fence delimiter).
//
// One derivation rule for the TAG-NAME alphabet:
//
//	C. For every rune of every guarded tag name, every rune whose NFKC form is
//	   exactly that rune. U+2F64 KANGXI RADICAL USE ⽤ folds to 用, so `</⽤数据>`
//	   is the same failure mode as the delimiter case, one alphabet over. Rule C
//	   closes it by construction: the guarded names live here, and re-running the
//	   generator finds new members.
//
// Rule C is NFKC-only, so it cannot reach traditional/simplified Han variants — those
// are listed explicitly in guardedTagVariants and merged into the same table. See that
// declaration for why the derived set needs a hand-listed supplement in exactly this
// one place.
//
// The generated set covers the DECLARED MECHANICAL POLICY above — NFKC equivalence
// plus two Unicode-name heuristics — not every visually confusable character. A rune
// that looks angle-like but whose name matches neither heuristic (U+22D6 LESS-THAN
// WITH DOT, arrowhead modifier letters) is out of scope by construction and is
// accepted residual: such runes are distinct tokens that do not read as a bare fence.
// This is a policy boundary, not an exhaustive-confusables claim; widening the policy
// means editing the rules here and re-running, which is the point of deriving it.
//
// "ANGLE QUOTATION MARK" counts as an angle bracket for rule B: U+2039/U+203A
// (‹›) and U+276E/U+276F (❮❯) are named as quotation marks but render as bare
// angle brackets, so a reader — and a model — sees `‹/引用数据›` as the fence.
// DOUBLE ANGLE forms («») stay excluded: two chevrons do not read as one bracket.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/unicode/runenames"
)

// excluded name fragments: glyphs whose names match the angle/solidus heuristics but
// that do not read as a bare delimiter, plus REVERSE SOLIDUS which is a different
// character.
//
// The rule here is "exclude when the decoration changes what a reader sees as the
// token", NOT "exclude everything decorated". `《》` (DOUBLE ANGLE) is out because two
// chevrons do not read as one bracket, and enclosed/circled forms are out because the
// enclosure dominates the glyph. But `⦑` (LEFT ANGLE BRACKET WITH DOT), `⫽` (DOUBLE
// SOLIDUS OPERATOR) and `⹊` (DOTTED SOLIDUS) ARE kept: the dot or the doubling does
// not stop them reading as a bracket or a slash. Erring inclusive is the safe
// direction here — a false positive costs one substituted rune inside a tag pattern,
// a false negative is a fence escape.
var excluded = []string{
	"OVERLAY", "CIRCLE", "CIRCLED", "SQUARED", "APL ", "MUSICAL", "OCR ",
	"MODIFIER", "KANGXI", "TAG ", "INTEGRAL", "PRECEDING", "DOUBLE ANGLE",
	"OVERBAR", "HORIZONTAL STROKE", "NEGATION", "BINARY RELATION", "DIVIDE",
	"REVERSE SOLIDUS", "BACKSLASH",
}

// guardedTagNames lists every tag name protected by a fenceGuard. Rule C derives
// the tag-name homoglyph folds from these, so adding a guard means adding its name
// here and re-running the generator — not hand-writing a new homoglyph list.
//
// Only <引用数据> is here. The document AI preview endpoint carries untrusted
// document text in its own chat message rather than inside an in-band fence
// (see documentPreviewInstruction), so it has no delimiter to forge and needs no
// guard. Keep in sync with the newFenceGuard call sites;
// TestFenceTagNameFoldsCoverGuardedNames pins it.
var guardedTagNames = []string{"引用数据"}

// guardedTagVariants are additional SPELLINGS of a guarded name that rule C cannot
// reach, listed explicitly.
//
// Rule C is NFKC-preimage only, and traditional/simplified Han variants are not
// NFKC-equivalent: `數` is not a compatibility form of `数`. So re-running the
// generator can never discover `引用數據`, and the PR's own thesis — that a
// hand-written list is a set the attacker picks from the complement — applies to the
// derived set too. For a CJK tag name in a Chinese-language product, 繁/简 variants
// are the most obvious member of that complement: copying a traditional spelling
// takes no effort and no knowledge.
//
// These are folded ONTO the canonical runes, so they join the same in-pattern
// alternation as the NFKC preimages. TestFenceGuardHanVariantsAreGuarded pins them.
//
// This list is a DECISION, not a derivation, and it is the one place in this file
// that is. It is small and it is stated; the alternative was leaving a documented
// hole for the most likely spelling an attacker would reach for.
var guardedTagVariants = map[string]string{
	"數": "数", // U+6578 traditional 'number'
	"據": "据", // U+64DA traditional 'according to'
}

type entry struct {
	r         rune
	to        string
	rule      string
	unicodeNm string
}

func main() {
	// nameRunes is the set of runes appearing in any guarded tag name (rule C targets).
	nameRunes := map[rune]bool{}
	for _, tag := range guardedTagNames {
		for _, r := range tag {
			nameRunes[r] = true
		}
	}

	var entries []entry
	var nameEntries []entry
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // surrogates
		}
		name := runenames.Name(r)
		if name == "" || strings.HasPrefix(name, "<") {
			continue // unassigned / control ranges
		}

		// Rule C: NFKC preimages of the guarded tag-name runes.
		if !nameRunes[r] {
			if nf := norm.NFKC.String(string(r)); len([]rune(nf)) == 1 && nameRunes[[]rune(nf)[0]] {
				nameEntries = append(nameEntries, entry{r, nf, "NFKC-name", name})
			}
		}

		if r == '<' || r == '>' || r == '/' {
			continue // already canonical
		}

		if nf := norm.NFKC.String(string(r)); nf == "<" || nf == ">" || nf == "/" {
			entries = append(entries, entry{r, nf, "NFKC", name})
			continue
		}

		isAngle := strings.Contains(name, "ANGLE BRACKET") || strings.Contains(name, "ANGLE QUOTATION MARK")
		isSolidus := strings.Contains(name, "SOLIDUS") || strings.Contains(name, "SLASH")
		if !isAngle && !isSolidus {
			continue
		}
		skip := false
		for _, frag := range excluded {
			if strings.Contains(name, frag) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		var to string
		switch {
		case isSolidus:
			to = "/"
		case strings.Contains(name, "LEFT"):
			to = "<"
		case strings.Contains(name, "RIGHT"):
			to = ">"
		default:
			continue
		}
		entries = append(entries, entry{r, to, "name", name})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].r < entries[j].r })

	// Merge the explicitly-listed Han variants into the tag-name entries. They cannot
	// be derived (see guardedTagVariants), so they are appended and then sorted with
	// the derived ones — the table stays a single ordered list regardless of origin,
	// and the `rule` column records which mechanism produced each row.
	for from, to := range guardedTagVariants {
		fromRunes := []rune(from)
		toRunes := []rune(to)
		if len(fromRunes) != 1 || len(toRunes) != 1 {
			fmt.Fprintf(os.Stderr, "guardedTagVariants: %q -> %q must both be single runes\n", from, to)
			os.Exit(1)
		}
		if !nameRunes[toRunes[0]] {
			fmt.Fprintf(os.Stderr, "guardedTagVariants: %q folds to %q, which is not in any guarded tag name\n", from, to)
			os.Exit(1)
		}
		nameEntries = append(nameEntries, entry{fromRunes[0], to, "Han-variant", runenames.Name(fromRunes[0])})
	}
	sort.Slice(nameEntries, func(i, j int) bool { return nameEntries[i].r < nameEntries[j].r })

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Code generated by gen_fence_delims.go. DO NOT EDIT.\n")
	fmt.Fprintf(&buf, "// Regenerate with: go generate ./internal/api/handler\n\n")
	fmt.Fprintf(&buf, "package handler\n\n")
	fmt.Fprintf(&buf, "// fenceDelimiterFolds maps angle-bracket and solidus homoglyphs onto their\n")
	fmt.Fprintf(&buf, "// ASCII structural equivalent, as strings.NewReplacer pairs.\n")
	fmt.Fprintf(&buf, "// Derived from Unicode properties; %d entries.\n", len(entries))
	fmt.Fprintf(&buf, "var fenceDelimiterFolds = []string{\n")
	for _, e := range entries {
		fmt.Fprintf(&buf, "\t%q, %q, // U+%04X %s [%s]\n", string(e.r), e.to, e.r, e.unicodeNm, e.rule)
	}
	fmt.Fprintf(&buf, "}\n\n")

	fmt.Fprintf(&buf, "// fenceTagNameFolds maps compatibility homoglyphs of the guarded tag-name runes\n")
	fmt.Fprintf(&buf, "// onto those runes (rule C). Without this the tag NAME is matched as exact literal\n")
	fmt.Fprintf(&buf, "// runes, and a homoglyph spelling that is visually identical to a real closing\n")
	fmt.Fprintf(&buf, "// fence reaches the model untouched.\n")
	fmt.Fprintf(&buf, "// Derived from Unicode properties; %d entries; guarded names: %s.\n", len(nameEntries), strings.Join(guardedTagNames, ", "))
	fmt.Fprintf(&buf, "var fenceTagNameFolds = []string{\n")
	for _, e := range nameEntries {
		fmt.Fprintf(&buf, "\t%q, %q, // U+%04X %s [%s]\n", string(e.r), e.to, e.r, e.unicodeNm, e.rule)
	}
	fmt.Fprintf(&buf, "}\n\n")

	fmt.Fprintf(&buf, "// fenceGeneratedGuardedTagNames is the generator's view of which guards exist.\n")
	fmt.Fprintf(&buf, "// A guard whose name is missing here has NO tag-name fold coverage, so\n")
	fmt.Fprintf(&buf, "// TestFenceTagNameFoldsCoverGuardedNames fails rather than letting that ship.\n")
	fmt.Fprintf(&buf, "var fenceGeneratedGuardedTagNames = []string{\n")
	for _, tag := range guardedTagNames {
		fmt.Fprintf(&buf, "\t%q,\n", tag)
	}
	fmt.Fprintf(&buf, "}\n")

	src, err := format.Source(buf.Bytes())
	if err != nil {
		fmt.Fprintln(os.Stderr, "format:", err)
		os.Exit(1)
	}
	if err := os.WriteFile("fence_delims_table.go", src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote fence_delims_table.go (%d delimiter, %d tag-name entries)\n", len(entries), len(nameEntries))
}
