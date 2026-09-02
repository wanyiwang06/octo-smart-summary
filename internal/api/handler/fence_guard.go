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
//     required, and replaces every opener rune in the match with `[`. It therefore
//     carries NO length bound and NO line-break exclusion, which is what makes it a
//     true fallback: there is no attacker-selectable number to pad past, and no
//     padding SHAPE that both passes decline.
//   - tagPattern is a COSMETIC pass over well-formed tags only. Its character
//     classes admit nothing but delimiters, separators, zero-width runes and the tag
//     name itself, so it cannot swallow a clause even in principle — the failure mode
//     where a stray `<` and a `>` two paragraphs later deleted everything between
//     them is not expressible in this grammar, because the grammar has no "any rune"
//     component. Its one bound (fenceMaxSepRun) is a FAITHFULNESS parameter, not a
//     security boundary: exceeding it declines the cosmetic rewrite and falls to
//     headPattern, which has no bound at all.
//
// The relationship between the two passes is the security argument, so it is stated
// once here precisely: headPattern's match set is a strict SUPERSET of the openers
// tagPattern can match. Any shape tagPattern declines — for any reason, including
// every bound it carries — is still reached by headPattern. An earlier revision
// broke exactly this property by excluding `\n` from headPattern's pre-name region
// while allowing it between tag-name runes, which left 32 newline-padded shapes
// matched by neither pass; TestFenceGuardHeadPassIsTrueFallback pins it now.
//
// # Why the delimiter alphabet is matched, not folded
//
// Both alphabets — delimiters and tag-name runes — are applied as ALTERNATIONS
// INSIDE the patterns, never as a global rewrite of the text.
//
// This is not a style choice. An earlier revision folded the 31 delimiter homoglyphs
// globally in normalize(), which rewrote legitimate content: 〈〉 (U+3008/U+3009) are
// 单书名号, ordinary Chinese punctuation for article and chapter titles, so
// `推荐阅读〈论持久战〉` came back as `推荐阅读<论持久战>`. Likewise `1⁄2` → `1/2`
// (U+2044 FRACTION SLASH), `‹bonjour›` → `<bonjour>` (French guillemets), and
// `︿︿` → `<<` (common CJK IM emoticon eyes). This is a Chinese-language
// summarizer whose entire job is to reproduce quoted text verbatim, and the mutated
// text is what the model quotes back to users.
//
// Matching more inside a pattern costs the text nothing. Folding globally corrupts
// it. The coverage is identical either way.
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

// The cosmetic pass reasons about a single line terminator, `\n`, because
// normalize() folds every other line/paragraph separator INTO it (see
// fenceControlFolds). Folding them to a SPACE, as the predecessor did, would make any
// line-break reasoning unreachable for eight of the nine terminator spellings, and
// would silently turn a quoted paragraph break into prose.

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
	// It is a FAITHFULNESS parameter, not a security boundary, and the distinction is
	// structural rather than asserted: exceeding it declines the cosmetic rewrite and
	// falls to headPattern, whose gaps AND pre-name region are both unbounded and both
	// admit line breaks, so it neutralizes the opener at any padding width or shape.
	// What the bound buys is that the cosmetic pass cannot collapse an arbitrarily long
	// run of separators — blank lines included — into a 6-rune placeholder.
	//
	// Note that this is the ONLY thing standing between the cosmetic pass and a
	// paragraph collapse, and it is sufficient: that pass has no unbounded "any rune"
	// region anywhere, so bounding the separator run bounds everything it can absorb.
	// An explicit line-break exclusion would be redundant here and actively harmful —
	// it would decline the cosmetic rewrite for the ordinary `</引用\n数据>` chunk-join
	// shape and emit the uglier in-place form for it.
	//
	// An earlier revision made the "not a security boundary" claim while headPattern's
	// pre-name region excluded `\n`, which made the fallback conditional and the claim
	// false for 32 shapes. TestFenceGuardHeadPassIsTrueFallback now pins the property
	// the claim depends on.
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
	// Legacy bidi embeddings/overrides. Deprecated in favour of the isolates above,
	// but still present in real stored text, and stripping them reorders the visible
	// string exactly as stripping LRM/RLM does.
	"\u202a": true, // LEFT-TO-RIGHT EMBEDDING
	"\u202b": true, // RIGHT-TO-LEFT EMBEDDING
	"\u202c": true, // POP DIRECTIONAL FORMATTING
	"\u202d": true, // LEFT-TO-RIGHT OVERRIDE
	"\u202e": true, // RIGHT-TO-LEFT OVERRIDE
	// Invisible mathematical operators carry SEMANTICS rather than layout: U+2061 is
	// function application and U+2062 an implied multiplication sign, so removing them
	// changes what a quoted formula MEANS, not how it renders.
	"\u2061": true, // FUNCTION APPLICATION
	"\u2062": true, // INVISIBLE TIMES
	"\u2063": true, // INVISIBLE SEPARATOR
	"\u2064": true, // INVISIBLE PLUS
}

// fenceControlFolds collapse the separators that would otherwise let a tag straddle
// a line or record boundary invisibly.
//
// Line and paragraph separators fold to `\n`, NOT to a space. Two reasons, and the
// second is why the direction matters: it keeps a quoted paragraph break a paragraph
// break instead of silently turning it into prose, and it collapses nine terminator
// spellings into one so nothing downstream has to enumerate them. Tab and NUL fold to
// a space because they are not line breaks.
//
// Note what is NOT here any more: the delimiter homoglyphs. Folding those globally
// rewrote legitimate content (〈〉 单书名号, `1⁄2`, `‹bonjour›`, `︿︿`), so they are
// matched in-pattern instead — see fenceDelimClass and the type docstring.
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

var fenceControlReplacer = strings.NewReplacer(fenceControlFolds...)

// fenceDelimiterFoldMap inverts the generated fenceDelimiterFolds pairs into
// canonical-delimiter -> {homoglyph runes}. Built once at init.
var fenceDelimiterFoldMap = func() map[rune][]rune {
	m := map[rune][]rune{}
	for i := 0; i+1 < len(fenceDelimiterFolds); i += 2 {
		from := []rune(fenceDelimiterFolds[i])
		to := []rune(fenceDelimiterFolds[i+1])
		if len(from) != 1 || len(to) != 1 {
			continue
		}
		m[to[0]] = append(m[to[0]], from[0])
	}
	return m
}()

// fenceDelimClass renders one canonical delimiter as a character-class BODY holding
// itself and every generated homoglyph of it, for embedding inside `[...]`.
//
// This is the P1-2 mechanism: the delimiter alphabet is matched here rather than
// folded across the whole text, for exactly the reason the tag-name alphabet always
// was. See the type docstring for the content that a global fold corrupted.
func fenceDelimClass(canonical rune) string {
	var b strings.Builder
	b.WriteString(regexp.QuoteMeta(string(canonical)))
	for _, r := range fenceDelimiterFoldMap[canonical] {
		b.WriteString(regexp.QuoteMeta(string(r)))
	}
	return b.String()
}

// The three delimiter classes, as character-class bodies and as complete classes.
//
// The bodies exist separately so they can be composed into NEGATED classes ("any
// rune that is not an opener or a word rune") as well as positive ones.
var (
	fenceOpenBody  = fenceDelimClass('<')
	fenceCloseBody = fenceDelimClass('>')
	fenceSlashBody = fenceDelimClass('/')

	fenceOpenClass  = `[` + fenceOpenBody + `]`
	fenceCloseClass = `[` + fenceCloseBody + `]`
	fenceSlashClass = `[` + fenceSlashBody + `]`

	// fenceOpenRunes is the set form, used by neutralizeFenceOpeners to rewrite every
	// opener SPELLING in a head match, not just ASCII `<`. Once openers are no longer
	// folded to `<` up front, neutralizing only `<` would leave `⟨引用数据` untouched.
	fenceOpenRunes = func() map[rune]bool {
		m := map[rune]bool{'<': true}
		for _, r := range fenceDelimiterFoldMap['<'] {
			m[r] = true
		}
		return m
	}()
)

// fenceContainsOpener reports whether s holds any opener spelling. It replaces the
// `strings.ContainsRune(s, '<')` fast path, which was only correct while every
// opener homoglyph was folded to ASCII `<` before matching.
func fenceContainsOpener(s string) bool {
	for _, r := range s {
		if fenceOpenRunes[r] {
			return true
		}
	}
	return false
}

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

// fenceOpenNeutralized replaces an opener that would otherwise open a forged fence.
//
// It is exactly one rune, which is what lets the head pass rewrite in place instead
// of deleting, and it is chosen from OUTSIDE the delimiter alphabet: `[` is not a
// fold source for `<` in fenceDelimiterFolds, so neutralizing can never manufacture
// a new opener. That property now covers 31 generated runes rather than 3, so it is
// asserted mechanically by TestFenceNeutralizerIsNotAnOpener rather than by reading
// the table. It also matches the bracket the placeholder already uses.
const fenceOpenNeutralized = "["

// fenceMaxRewritePasses caps the fixpoint loop.
//
// The cap is safe only because the head pass converges in a single pass BY
// CONSTRUCTION: it neutralizes every opener inside its own match, including the
// preserved boundary rune, so its output cannot contain an opener adjacent to a tag
// name for a later pass to find. The loop therefore exists only for the cosmetic
// pass's shortening rewrites, where 2 iterations suffice today.
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
	fenceTagPad := fenceTagGap + `(?:` + fenceSlashClass + fenceTagGap + `){0,2}`

	// The head pass's prefix. Fully unbounded, because this pass deletes nothing:
	// crossing more text costs one substituted rune, not a clause.
	//
	// It deliberately does NOT exclude line breaks. An earlier revision excluded `\n`
	// here while fenceHeadGap (between tag-name runes) admitted it via \p{Cc}, and that
	// asymmetry was a containment hole, not a safety margin: `<\n\n\n/引用数据>` and
	// `<\n///引用数据>` were declined by the cosmetic pass (too many separators / too
	// many solidi) AND unreachable for the head pass, so they shipped byte-identical —
	// 32 such shapes, two of which the previous hand-written guard had blocked. Worse,
	// sanitizeRefLine then folded that `\n` to a space and reconstituted a well-formed
	// single-line fence.
	//
	// Nothing is lost by admitting line breaks here. The rule "a fence tag is a token,
	// and a token does not span paragraphs" exists to stop a pass from COLLAPSING a
	// paragraph, and this pass cannot collapse anything — it substitutes one rune for
	// one rune. On the cosmetic pass, which can shorten, the same protection comes from
	// fenceMaxSepRun bounding every separator run it may absorb.
	//
	// Adjacency decides token identity, which is what keeps ordinary text intact:
	// `<0引用数据` stays as-is (`0` continues the name), while `<0/引用数据>` is a tag.
	// Excluding the closer throughout stops a stray opener reaching across an
	// already-closed tag.
	fenceHeadPathNoise := `[^` + fenceWordClass + fenceCloseBody + `\p{Z}\p{Cc}]*`
	fenceHeadDelimNoise := `[^` + fenceWordClass + fenceCloseBody + `]*`
	fenceHeadPrefix := `(?:` + fenceHeadPathNoise + `[` + fenceWordClass + `]+` + fenceSlashClass + `)*` + fenceHeadDelimNoise

	return &fenceGuard{
		tagName: tagName,
		tagPattern: regexp.MustCompile(
			fenceOpenClass + fenceTagPad + buildName(fenceTagGap) + fenceTagPad + fenceCloseClass),
		placeholder: "[" + tagName + "]",
		// The boundary condition is deliberately NEGATIVE — "the tag name is not
		// continued by another letter or digit" — rather than an allow-list of
		// delimiters. An allow-list is a bound the attacker picks: with `[\s\p{Zs}/>]`,
		// `</引用数据">` (one punctuation rune) matches nothing and ships verbatim. Since
		// these tag names are CJK, the only thing that makes `<引用数据…` a different
		// TOKEN is a letter/digit continuation.
		headPattern: regexp.MustCompile(
			fenceOpenClass + fenceHeadPrefix + buildName(fenceHeadGap) + `([^` + fenceWordClass + `]|$)`),
	}
}

// normalize folds control separators and strips globally-safe invisibles.
//
// It does NOT fold the delimiter alphabet: those are matched in-pattern, because a
// global fold rewrote legitimate content. See the type docstring.
func (g *fenceGuard) normalize(s string) string {
	s = fenceControlReplacer.Replace(s)
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
// leaves the cosmetic pass's line-break exclusion unable to see anything, so a tag
// split across a line boundary is not bounded.
//
// Budget invariant relied on by callers that pre-compute a rune budget: this can
// never LENGTHEN the text. It is not strictly shortening: the cosmetic pass's
// shortest match `<tagName>` is 6 runes and maps to a placeholder of exactly 6, and
// the head pass substitutes one rune for one rune. Non-expansion is the property a
// caller needs and the only one guaranteed.
//
// Both passes run to a FIXPOINT. Preserving the boundary rune is what keeps
// surrounding text intact, but the boundary rune can itself be an opener, and then
// the replacement sits next to it as a fresh tag: `<引用数据<引用数据` leaves
// `引用数据<引用数据` after one pass — an intact tag, and a violation of idempotence.
// Looping is the structural answer rather than another pattern edit: it terminates
// because every rewrite strictly reduces the number of opener runes.
func (g *fenceGuard) neutralize(s string) string {
	s = g.normalize(s)
	for i := 0; i < fenceMaxRewritePasses; i++ {
		if !fenceContainsOpener(s) {
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

// neutralizeFenceOpeners rewrites one head match in place, replacing every OPENER
// SPELLING it contains with fenceOpenNeutralized and preserving every other rune.
//
// Replacing ALL of them, not just the leading one, is what makes the pass idempotent
// in a single application: the match can contain further openers in its prefix or in
// the preserved boundary rune, and leaving any of them would hand the next pass a
// fresh candidate — the `<引用数据<引用数据` shape.
//
// Accepted collateral damage, recorded as a decision: the preserved boundary rune is
// rewritten too, so `<引用数据<br>` becomes `[引用数据[br>`. Markup inside untrusted
// quoted text is not content this product promises to reproduce, and single-pass
// idempotence is worth more than that markup. TestFenceGuardBoundaryRuneCollateral
// pins it.
func neutralizeFenceOpeners(match string) string {
	var b strings.Builder
	b.Grow(len(match))
	for _, r := range match {
		if fenceOpenRunes[r] {
			b.WriteString(fenceOpenNeutralized)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
