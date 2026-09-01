package handler

import (
	"regexp"
	"strings"
)

//go:generate go run gen_fence_delims.go

// fenceGuard neutralizes forged fence tags for exactly one tag name.
//
// # Why this file exists
//
// `<引用数据>…</引用数据>` fences untrusted referenced-summary text in the agent's
// system prompt, and the header contract tells the model that everything between
// those markers is verbatim DATA it must never obey (SUM-158 blocker 3). A
// referenced summary quotes arbitrary chat content authored by other people, so
// that text must not be able to forge a closing marker and escape into the
// instruction region.
//
// The hand-written predecessor folded three runes — ＜ ＞ ／ — before matching the
// tag literally. Everything else went through byte-identical:
//
//	⟨/引用数据⟩   U+27E8/U+27E9 MATHEMATICAL LEFT/RIGHT ANGLE BRACKET
//	〈/引用数据〉   U+2329/U+232A LEFT/RIGHT-POINTING ANGLE BRACKET
//	﹤/引用数据﹥   U+FE64/U+FE65 SMALL LESS-THAN/GREATER-THAN SIGN
//	<//引用数据>   doubled solidus
//	</引用数据 x>  attribute tail
//	</引用\u00a0数据>  NBSP inside the name
//
// A hand-written alphabet is a set the attacker picks from the complement, so the
// alphabet here is DERIVED from Unicode properties by gen_fence_delims.go instead
// (fence_delims_table.go). New members arrive by re-running the generator.
//
// # Why nothing here deletes prose
//
// This guard runs on the text the product exists to reproduce faithfully. A
// sanitizer that over-matches is not "safe by default" for a summarizer: it
// silently corrupts the quoted content, with no truncation marker to tell anyone
// it happened. So containment is carried entirely by a pass that rewrites IN
// PLACE — one rune substituted for one rune, nothing removed:
//
//   - headPattern matches an opener adjacent to the tag name, with no closing `>`
//     required, and replaces every `<` in the match with `[`. It therefore needs no
//     length bounds anywhere, which means there is no attacker-selectable number to
//     pad past to reach the model unchanged.
//   - tagPattern is a COSMETIC pass over well-formed tags only. Its character
//     classes admit nothing but delimiters, separators and the tag name itself, so
//     it cannot swallow a clause even in principle — the failure mode where a stray
//     `<` and a `>` two paragraphs later deleted everything between them is not
//     expressible in this grammar, because the grammar has no "any rune" component.
//
// There is deliberately no attribute-tail construct and no prose-crossing prefix on
// the deleting pass. Anything tagPattern declines falls through to headPattern and
// costs the text exactly one substituted rune.
type fenceGuard struct {
	tagName string
	// tagPattern matches a well-formed tag and rewrites it to placeholder. Cosmetic:
	// it exists so ordinary forged tags read as [引用数据] rather than [/引用数据>.
	tagPattern *regexp.Regexp
	// headPattern matches the tag HEAD alone and is rewritten in place. This is the
	// pass that carries containment.
	headPattern *regexp.Regexp
	// placeholder is the non-empty replacement. Non-empty is load-bearing twice: it
	// keeps the injection visible to the model as neutralized text, and it prevents
	// split-token reassembly, where deleting a tag splices its neighbours together
	// into a fresh copy of the same token.
	placeholder string
}

// fenceWordClass is the letter/digit class that decides token continuation, written
// as a character-class body so it can be embedded with and without negation.
const fenceWordClass = `\p{L}\p{Nd}`

// fenceLineBreak is the single line-terminator this file has to reason about.
//
// normalize() folds every other line/paragraph separator INTO it (see
// fenceControlFolds), which is what makes the line-break exclusions below
// reachable. The predecessor folded them to a SPACE instead, so `\n` was the only
// terminator any exclusion could actually see and `\u2028` / `\u2029` / `\u0085`
// slipped past every one of them.
const fenceLineBreak = `\n`

// Zero-width and separator classes for the gaps BETWEEN tag-name runes.
//
// Runes inserted inside a tag name break the literal rune run that structural
// matching depends on without changing how a model reads the token:
//
//   - ZERO-WIDTH: format controls (Cf), non-spacing marks (Mn — variation
//     selectors, the combining grapheme joiner U+034F, combining accents),
//     enclosing marks (Me), and the soft hyphen. They render as nothing.
//   - SEPARATORS: whitespace (Z, including U+3000 ideographic space) and C0/C1
//     controls (Cc, which is what carries a bare `\n`). `</引用\n数据>` is reachable
//     with no effort at all, because quoted chunks are joined with `\n` — an
//     attacker only has to split the tag across a chunk boundary. These tag names
//     are CJK, which has no word spacing, so a separator between two of their runes
//     is not a token boundary the way it would be in Latin prose: `</引用 数据>`
//     reads as the same closing fence.
//
// These classes apply ONLY between the runes of a tag name, never globally. A
// global \p{Mn} strip would corrupt legitimate decomposed text (café, Việt), and the
// quoted body is content to be reproduced, not syntax.
//
// Deliberate residual: \p{Mc} (spacing combining marks, e.g. U+0903) is not here.
// Those render visibly, so `</引ः用数据>` is a visibly different tag name — the same
// residual class as `<引用数据格式说明>`, a different token. A decision, not an
// oversight.
const (
	fenceZeroWidthSet = `[\p{Cf}\p{Mn}\p{Me}\x{00AD}]`
	fenceSepClass     = `[\p{Z}\p{Cc}]`
	// fenceMaxSepRun bounds the separator run per gap on the COSMETIC pass only.
	//
	// It is not a security boundary of the kind an attacker pads past: exceeding it
	// does not deliver a clean fence, it declines the cosmetic rewrite and falls to
	// headPattern, which has no bound and neutralizes the opener at any padding
	// width. What the bound buys is that the cosmetic pass cannot collapse an
	// arbitrarily long run of blank lines into a 6-rune placeholder.
	fenceMaxSepRun = 2
)

// fenceZeroWidthClass is the unbounded form, used by the in-place head pass.
const fenceZeroWidthClass = fenceZeroWidthSet + `*`

// fenceTagGap is one inter-rune gap for the cosmetic pass: unbounded zero-width
// runes (they render as nothing, so absorbing them loses no visible text)
// interleaved with at most fenceMaxSepRun visible separators.
const fenceTagGap = fenceZeroWidthClass + `(?:` + fenceSepClass + fenceZeroWidthClass + `){0,2}`

// fenceHeadGap is the same gap for the in-place pass, with the separator run
// unbounded. That pass deletes nothing, so it carries no bound — which is what
// keeps `</引用   数据>` (three spaces, two keystrokes past the cosmetic cap)
// covered rather than shipping byte-identical.
const fenceHeadGap = fenceZeroWidthClass + `(?:` + fenceSepClass + fenceZeroWidthClass + `)*`

// fenceGlobalInvisiblePattern is a pre-pass strip that shrinks the search space.
// It stays limited to Cf and the soft hyphen: those have no legitimate rendering
// role in prose, whereas Mn/Me do.
//
// fenceGlobalInvisibleKeep carves out the Cf runes that ARE load-bearing in real
// text. Stripping them globally corrupts the text being summarized, which is a
// faithfulness bug in a product whose whole job is to reproduce it:
//
//	U+200D ZWJ   — joins family emoji and Devanagari conjuncts
//	U+200C ZWNJ  — ORTHOGRAPHIC in Persian and Devanagari: میخوانم and می‌خوانم
//	               are different words, not two renderings of one word
//	U+200E LRM / U+200F RLM — bidi ordering in mixed Arabic/Hebrew + Latin text;
//	               stripping them silently reorders the visible string
//	U+2060 WORD JOINER — a no-break point; removing it changes line breaking
//
// The carve-out costs no containment, because fenceZeroWidthSet (which includes ALL
// of Cf) still neutralizes these INSIDE a tag name: `</\u200c引用数据>` and
// `</引用\u200c数据>` are both caught by the patterns themselves. The global pass is
// an optimization, not a containment mechanism.
var fenceGlobalInvisiblePattern = regexp.MustCompile(`[\p{Cf}\x{00AD}]`)

var fenceGlobalInvisibleKeep = map[string]bool{
	"\u200d": true, // ZERO WIDTH JOINER
	"\u200c": true, // ZERO WIDTH NON-JOINER
	"\u200e": true, // LEFT-TO-RIGHT MARK
	"\u200f": true, // RIGHT-TO-LEFT MARK
	"\u2060": true, // WORD JOINER
	// Bidi isolates are the modern replacement for LRM/RLM and do MORE reordering,
	// not less, so the carve-out's own rationale applies to them a fortiori.
	"\u2066": true, // LEFT-TO-RIGHT ISOLATE
	"\u2067": true, // RIGHT-TO-LEFT ISOLATE
	"\u2068": true, // FIRST STRONG ISOLATE
	"\u2069": true, // POP DIRECTIONAL ISOLATE
	"\u061c": true, // ARABIC LETTER MARK
}

// fenceControlFolds collapse the separators that would otherwise let a tag straddle
// a line or record boundary invisibly.
//
// Line and paragraph separators fold to `\n`, NOT to a space. That direction is the
// whole point: the patterns below exclude line breaks so a fence tag cannot span
// paragraphs, and folding U+2028/U+2029/U+0085 to a space would make those
// exclusions unreachable for every terminator except `\n` — a guard whose stated
// property held for one of nine spellings. Tab and NUL fold to a space because they
// are not line breaks.
var fenceControlFolds = []string{
	"\r\n", "\n",
	"\r", "\n",
	"\v", "\n",
	"\f", "\n",
	"\u0085", "\n",
	"\u2028", "\n",
	"\u2029", "\n",
	"\t", " ",
	"\x00", " ",
}

var fenceDelimiterReplacer = strings.NewReplacer(
	append(append([]string{}, fenceDelimiterFolds...), fenceControlFolds...)...,
)

// fenceTagNameFoldMap inverts the generated fenceTagNameFolds pairs into
// canonical-rune -> {homoglyph runes}. Built once at init.
var fenceTagNameFoldMap = func() map[rune][]rune {
	m := map[rune][]rune{}
	for i := 0; i+1 < len(fenceTagNameFolds); i += 2 {
		from := []rune(fenceTagNameFolds[i])
		to := []rune(fenceTagNameFolds[i+1])
		if len(from) != 1 || len(to) != 1 {
			continue
		}
		m[to[0]] = append(m[to[0]], from[0])
	}
	return m
}()

// fenceTagRuneAlternation renders one tag-name rune as a regex alternation of
// itself and every generated homoglyph of it.
//
// Alternation rather than a global fold is deliberate: folding ⽤→用 across the text
// would rewrite the content being summarized, which is the faithfulness failure this
// file is organized to avoid. Matching more inside the tag pattern costs nothing.
func fenceTagRuneAlternation(r rune) string {
	alts := fenceTagNameFoldMap[r]
	if len(alts) == 0 {
		return regexp.QuoteMeta(string(r))
	}
	parts := make([]string, 0, len(alts)+1)
	parts = append(parts, regexp.QuoteMeta(string(r)))
	for _, a := range alts {
		parts = append(parts, regexp.QuoteMeta(string(a)))
	}
	return `(?:` + strings.Join(parts, `|`) + `)`
}

// fenceOpenNeutralized replaces a `<` that would otherwise open a forged fence.
//
// It is exactly one rune, which is what lets the head pass rewrite in place instead
// of deleting, and it is chosen from OUTSIDE the delimiter alphabet: `[` is not a
// fold source for `<` in fenceDelimiterFolds, so neutralizing can never manufacture
// a new opener. It also matches the bracket the placeholder already uses.
const fenceOpenNeutralized = "["

// fenceMaxRewritePasses caps the fixpoint loop.
//
// The cap is safe only because the head pass converges in a single pass BY
// CONSTRUCTION: it neutralizes every `<` inside its own match, including the
// preserved boundary rune, so its output cannot contain a `<` adjacent to a tag name
// for a later pass to find. The loop therefore exists only for the cosmetic pass's
// shortening rewrites, where 2 iterations suffice today.
//
// A cap alone would not be a fix. An unbounded-prefix deleting pass combined with a
// loop bounded by len(s) makes `"<"*N + tagName` take N/9 full scans — O(N²), and
// measured at 683ms on this very path from ordinary IM message text. Convergence has
// to be structural first; the cap is then just an assertion that it is.
const fenceMaxRewritePasses = 8

func newFenceGuard(tagName string) *fenceGuard {
	// Interleave the gap class around every rune of the tag name, including a leading
	// position so `<\uFE0F引用数据>` is covered. Each rune is matched as an ALTERNATION
	// of itself and its NFKC preimages, so `</⽤数据>` is caught without rewriting a
	// legitimate ⽤ anywhere else in the text.
	buildName := func(gap string) string {
		var b strings.Builder
		b.WriteString(gap)
		for _, r := range tagName {
			b.WriteString(fenceTagRuneAlternation(r))
			b.WriteString(gap)
		}
		return b.String()
	}

	// The cosmetic pass's prefix and suffix. Note what is NOT here: no `[^>]*`, no
	// bounded "noise" run, no attribute tail. The classes admit only zero-width runes,
	// separators and solidi, so this pattern cannot match prose, and therefore cannot
	// delete prose — the property is structural rather than a number a test has to
	// defend. The separator run inside each gap stays capped at fenceMaxSepRun, so the
	// worst this pass can absorb is a handful of blank lines around a real tag.
	const fenceTagPad = fenceTagGap + `(?:/` + fenceTagGap + `){0,2}`

	// The head pass's prefix. Unbounded, because this pass deletes nothing: crossing
	// more text costs one substituted rune, not a clause. Line breaks stay excluded
	// for a reason unrelated to deletion — a fence tag is a token, and a token does
	// not span paragraphs.
	//
	// Adjacency decides token identity, which is what keeps ordinary text intact:
	// `<0引用数据` stays as-is (`0` continues the name), while `<0/引用数据>` is a tag.
	// Excluding `>` throughout stops a stray `<` reaching across an already-closed tag.
	const fenceHeadPathNoise = `[^` + fenceWordClass + `>\p{Z}\p{Cc}]*`
	const fenceHeadDelimNoise = `[^` + fenceWordClass + `>` + fenceLineBreak + `]*`
	const fenceHeadPrefix = `(?:` + fenceHeadPathNoise + `[` + fenceWordClass + `]+/)*` + fenceHeadDelimNoise

	return &fenceGuard{
		tagName:     tagName,
		tagPattern:  regexp.MustCompile(`<` + fenceTagPad + buildName(fenceTagGap) + fenceTagPad + `>`),
		placeholder: "[" + tagName + "]",
		// The boundary condition is deliberately NEGATIVE — "the tag name is not
		// continued by another letter or digit" — rather than an allow-list of
		// delimiters. An allow-list is a bound the attacker picks: with `[\s\p{Zs}/>]`,
		// `</引用数据">` (one punctuation rune) matches nothing and ships verbatim. Since
		// these tag names are CJK, the only thing that makes `<引用数据…` a different
		// TOKEN is a letter/digit continuation.
		headPattern: regexp.MustCompile(
			`<` + fenceHeadPrefix + buildName(fenceHeadGap) + `([^` + fenceWordClass + `]|$)`),
	}
}

// normalize folds delimiter homoglyphs and strips globally-safe invisibles.
func (g *fenceGuard) normalize(s string) string {
	s = fenceDelimiterReplacer.Replace(s)
	return fenceGlobalInvisiblePattern.ReplaceAllStringFunc(s, func(m string) string {
		if fenceGlobalInvisibleKeep[m] {
			return m
		}
		return ""
	})
}

// neutralize replaces forged fence tags with a non-empty placeholder, preserving
// line structure. Line and paragraph separators are normalized to `\n`; callers that
// render at single-value sites fold `\n` to a space themselves, AFTER this runs.
//
// That ordering is not incidental. Folding line breaks to spaces before the guard
// leaves the guard's line-break exclusions unable to see anything, so a tag split
// across a line boundary is neither matched nor bounded.
//
// Budget invariant relied on by callers that pre-compute a rune budget: this can
// only ever SHORTEN the text. The cosmetic pass's shortest match `<tagName>` is 2
// runes longer than tagName and maps to a placeholder exactly 2 runes longer;
// the head pass substitutes one rune for one rune.
//
// Both passes run to a FIXPOINT. Preserving the boundary rune is what keeps
// surrounding text intact, but the boundary rune can itself be a `<`, and then the
// replacement sits next to it as a fresh tag: `<引用数据<引用数据` leaves
// `引用数据<引用数据` after one pass — an intact tag, and a violation of idempotence.
// Looping is the structural answer rather than another pattern edit: it terminates
// because every rewrite strictly shortens or preserves length while strictly
// reducing the number of `<` runes.
func (g *fenceGuard) neutralize(s string) string {
	s = g.normalize(s)
	for i := 0; i < fenceMaxRewritePasses; i++ {
		if !strings.ContainsRune(s, '<') {
			return s
		}
		next := g.tagPattern.ReplaceAllString(s, g.placeholder)
		// Head pass runs second, over whatever the cosmetic pass declined, so ordinary
		// well-formed tags still render as the nicer [引用数据].
		next = g.headPattern.ReplaceAllStringFunc(next, neutralizeFenceOpeners)
		if next == s {
			return s
		}
		s = next
	}
	return s
}

// neutralizeFenceOpeners rewrites one head match in place, replacing every `<` it
// contains with fenceOpenNeutralized and preserving every other rune.
//
// Replacing ALL of them, not just the leading one, is what makes the pass idempotent
// in a single application: the match can contain further `<` runes in its prefix or
// in the preserved boundary rune, and leaving any of them would hand the next pass a
// fresh candidate — the `<引用数据<引用数据` shape.
func neutralizeFenceOpeners(match string) string {
	return strings.ReplaceAll(match, "<", fenceOpenNeutralized)
}
