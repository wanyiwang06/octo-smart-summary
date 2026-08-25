package handler

import (
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Centralized data-fence guard.
//
// Rounds 4–10 of #201 each widened one hand-written pattern and each left a new
// bypass reachable, in two alternating dimensions:
//
//   - the GRAMMAR dimension (what tail shapes count as a tag) — closed in round 8
//     by neutralizing the tag *head* with a negative boundary class, so no bound
//     on the tail is required at all;
//   - the ALPHABET dimension (which runes count as `<`, `/`, or "invisible") —
//     which was still a hand-written enumeration and is what round 10 broke, with
//     eleven confirmed forms reaching the model verbatim.
//
// The lesson both reviewers drew is the same: a set the author enumerates by hand
// is a set the attacker picks from the complement. So neither dimension is
// enumerated here any more.
//
//   - Delimiters come from fenceDelimiterFolds, DERIVED from Unicode properties by
//     gen_fence_delims.go (NFKC folding to `<`/`>`/`/`, plus characters whose
//     Unicode NAME makes them angle-bracket or solidus homoglyphs). Regenerate with
//     `go generate ./internal/api/handler`; do not hand-edit the table.
//   - TAG NAMES come from fenceTagNameFolds, derived by the same generator (rule C:
//     every rune whose NFKC form is a rune of a guarded tag name). Round 12 derived
//     the delimiter alphabet and stopped there, leaving the tag name matched as
//     exact literal runes — so `</⽂档数据>` (U+2F42 KANGXI RADICAL SCRIPT, visually
//     indistinguishable from 文) reached the model byte-identical. Same failure
//     mode, one alphabet over. The folds are applied as regex ALTERNATION inside the
//     tag-name pattern, never as a global rewrite of the text: the document body is
//     content to be summarized, so a legitimate ⽂ elsewhere in it must survive
//     byte-identical.
//   - Invisibles come from a general-category CLASS (Cf/Mn/Me/soft-hyphen), not a
//     list of codepoints, so variation selectors, combining marks, and the
//     not-yet-assigned members of those categories are covered by construction.
//
// Both guards in this package (<文档数据> and <引用数据>) are built from this one
// implementation, so a fix can no longer land on one and miss the other — that
// divergence was itself a finding in round 9.
//
// Known residual, deliberately out of scope at this layer: delimiters the model
// decodes but this pass never sees as `<`, i.e. entity/escape encodings
// (`&lt;/文档数据&gt;`, `\u003c`). Those are a decoding concern for whoever
// introduces a decoder upstream of the prompt builder; no amount of widening here
// addresses them. FuzzFenceGuard pins the properties this layer does claim.
//
// Known residual, RESIDUAL BYPASS REGISTER — forms that reach the model with `<`
// intact. Recorded so a reader of this header does not conclude the class is closed,
// and pinned by TestFenceGuardResidualBypassRegister so each entry is a decision
// rather than an accident. All of them apply to BOTH guards.
//
//	• A BARE WORD RUN in the prefix, not terminated by a solidus:
//	    `</abc 文档数据>`, `</ a 文档数据>`, `< a/文档数据>`
//	  The prefix admits a word run only when a `/` closes it (`<0/文档数据>`), because
//	  crossing bare words is what made round 11/12 delete prose. Since round 14 the
//	  head pass substitutes one rune instead of deleting, so that justification no
//	  longer applies to it and relaxing this is now a faithfulness trade rather than a
//	  deletion risk (`当 x < y 时，文档数据…` would become `当 x [ y 时，…`). Left as-is
//	  deliberately: the forms above carry visible junk between `<` and the name, the
//	  same residual class as `<文档数据格式说明>`.
//	• LETTER/DIGIT CONTINUATION of the tag name: `</文档数据abc>`, `</文档数据2>` — a
//	  different token by construction, per the negative boundary class.
//	• \p{Mc} SPACING combining marks inside the name: `</文ः档数据>` renders visibly
//	  differently, so it is a different tag name.
//
// Known COST, stated here because the "byte-identical" principle above is otherwise
// read as universal and it is not: fenceDelimiterReplacer is applied to the WHOLE
// text in normalize(), not just to tag neighbourhoods, so every angle-bracket and
// solidus homoglyph in the document body is folded to ASCII before the model sees
// it — 《》-adjacent book-title brackets 〈〉, math brackets ⟨⟩, single guillemets
// ‹›, and fraction/division slashes ⁄ ∕:
//
//	"参考〈左传〉与〈史记〉的记载"  ->  "参考<左传>与<史记>的记载"
//	"数学记号 ⟨a, b⟩ 表示内积"      ->  "数学记号 <a, b> 表示内积"
//
// CJK book-title brackets are ordinary prose in exactly the documents this feature
// targets, and the fold CREATES angle-bracket markup in the prompt that the author
// never wrote. This is a deliberate trade, not an oversight: the fold is what lets
// the patterns see one canonical `<`/`>` alphabet instead of enumerating homoglyphs
// at every match site, and narrowing it would reopen the round-9 delimiter class.
// The cost is confined to the PROMPT — the document itself is untouched, and the
// substitution is 1:1 so nothing is deleted or reordered. It applies equally to the
// already-shipped <引用数据> reference path.
// ---------------------------------------------------------------------------

//go:generate go run gen_fence_delims.go

// fenceWordClass is what counts as "continues the tag name into a different
// token". It is deliberately `\p{L}\p{Nd}` rather than `\p{L}\p{N}`: \p{N} also
// contains No/Nl (parenthesized and circled numerals such as U+2478 ⑸, roman
// numerals), which render as distinct decorative glyphs rather than as digits
// continuing a word. Fuzzing found `<⑸文档数据` shipping verbatim under the wider
// class. Narrowing it makes the guard neutralize MORE, which is the safe
// direction, and keeps this rule identical to the test oracle's
// unicode.IsLetter/unicode.IsDigit pair.
const fenceWordClass = `\p{L}\p{Nd}`

// Prefix bounds. See fencePrefix in newFenceGuard for why these are counts rather
// than the unbounded repetitions round 12 shipped.
//
// fenceMaxDeletion is the ceiling this file therefore commits to: the most runes a
// single match can consume beyond its own placeholder. It is what
// TestFenceGuardDeletionIsBounded and FuzzFenceGuard invariant 7 assert against, so
// changing any bound above without changing this one fails the suite.
const (
	fenceMaxPrefixNoise    = 8
	fenceMaxPrefixSegments = 3
	// fenceMaxTail bounds tagPattern's optional attribute/self-closing tail.
	//
	// Round 6 set this to 64 and the number was load-bearing then: the tag pass was
	// the only pass, so anything it declined shipped verbatim, and a bound the
	// attacker could pad past was a bypass. That is no longer true. Since round 14 the
	// head pass catches everything the tag pass declines and rewrites it IN PLACE, so
	// padding past this bound buys an inert head, not a fence.
	//
	// Which makes the tail purely a FAITHFULNESS trade, and 64 was the wrong side of
	// it: the tail DELETES, and 64 runes is enough to swallow a sentence. Round 15
	// measured `a < 文档数据 <60 runes of prose> >` ×100 losing 86% of the document with
	// no truncation marker — every per-match bound held, because the loss was spread
	// across matches.
	//
	// 16 covers real attribute syntax with room to spare (` attr=x` is 7, `/` is 1,
	// `\t attr` is 6, `"` is 1) while being too short to hold a clause. Anything longer
	// falls to the head pass and costs the document exactly one substituted rune.
	fenceMaxTail = 16

	fenceMaxPrefixNoiseStr    = "8"
	fenceMaxPrefixSegmentsStr = "3"
	fenceMaxTailStr           = "16"
)

// fenceMaxDeletion bounds the runes a single rewrite may remove, derived from the
// pattern bounds rather than measured, so the two cannot drift apart:
//
//	prefix   = (noise + word + "/") * segments + noise
//	tag name = len(name) runes, each gap absorbing at most fenceMaxSepRun separators
//	           and (fenceMaxSepRun + 1) * fenceMaxZeroWidthRun zero-width runes
//	tail     = fenceMaxTail runes + the tail's own lead rune + the `<` and `>`
//
// This is a PER-MATCH, PER-PASS bound. rewriteToFixpoint runs several passes, so
// the aggregate ceiling for a whole call is deletionBudget, which replays the
// passes and accumulates. Round 13 asserted the per-pass number against a
// multi-pass loss and the assertion was falsifiable by a 10-rune input.
func fenceMaxDeletion(tagName string) int {
	nameRunes := len([]rune(tagName))
	prefix := (fenceMaxPrefixNoise+fenceMaxPrefixNoise+1)*fenceMaxPrefixSegments + fenceMaxPrefixNoise
	perGap := fenceMaxSepRun + (fenceMaxSepRun+1)*fenceMaxZeroWidthRun
	gaps := (nameRunes + 1) * perGap
	const tail = fenceMaxTail + 1 + 2
	return prefix + gaps + nameRunes + tail
}

// itoa avoids importing strconv into a file that is otherwise pattern construction.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
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
// Alternation rather than a global fold is the whole point: folding ⽂→文 across the
// document would rewrite the text being summarized, which is exactly the
// faithfulness failure the prefix bound above exists to prevent. Matching more
// inside the tag pattern costs the document nothing.
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

// fenceIgnorableClass matches runes that can be inserted INSIDE a tag name to break
// the literal rune run that structural matching depends on, without changing how a
// model reads the token. It has two halves, deliberately bounded differently:
//
//   - ZERO-WIDTH runes — format controls (Cf), non-spacing marks (Mn: variation
//     selectors U+FE00–FE0F and U+E0100+, the combining grapheme joiner U+034F,
//     combining accents), enclosing marks (Me), and the soft hyphen. Unbounded: they
//     render as nothing, so however many the guard absorbs, no VISIBLE text is lost.
//   - SEPARATORS — whitespace (Z: space, U+3000 ideographic space, line/paragraph
//     separators) and C0/C1 controls (Cc, which is what carries a bare `\n`).
//     Bounded to fenceMaxSepRun per gap, because these DO occupy visible space and
//     absorbing an unbounded run of them is how round 12 deleted 4,003 runes of a
//     markdown table (see fencePrefix).
//
// The separator half closes the round-11 finding: `</文 档数据>`, `</文\u3000档数据>`
// and `</文\n档数据>` used to reach the model verbatim, and the last of those is
// reachable without any effort at all, because chunks are joined with `\n` — an
// attacker only has to split the tag across a chunk boundary. These tag names are
// CJK, which has no word spacing, so a separator between two of their runes is not
// a token boundary the way it would be in Latin prose: `</文 档数据>` reads as the
// same closing fence.
//
// fenceMaxSepRun = 2 is a coverage/faithfulness trade, not a security boundary that
// an attacker can pad past to reach the model unchanged: padding PAST it does not
// buy a clean fence, it buys a tag name visibly blown apart by whitespace
// (`</文    档数据>`), which is the same residual class as `<文档数据格式说明>` — a
// different token. The cheap and invisible forms (one `\n` from the chunk join, one
// space) stay covered.
//
// This class is applied ONLY between the runes of a tag name, never globally.
// A global \p{Mn} strip would corrupt legitimate decomposed text elsewhere in the
// document (café, Việt) — the document body is content to be summarized, not
// syntax, so it must survive byte-identical.
//
// Deliberate residual: \p{Mc} (spacing combining marks, e.g. U+0903) is NOT here.
// Those marks render visibly, so `</文ः档数据>` is a visibly different tag name, the
// same residual class as `<文档数据格式说明>`. Kept as a decision, not an oversight.
//
// fenceMaxZeroWidthRun bounds the run ONLY for the deleting pass. Round 13 left it
// unbounded on both, which let a single match absorb 5,000 Mn runes while
// fenceMaxDeletion charged for two — the arithmetic and the pattern disagreed, and
// the pattern won. The file's old justification ("they render as nothing, so no
// VISIBLE text is lost") is true of the runes themselves but not of the gap: an
// unbounded gap also swallows the separators interleaved with it. Bounding the
// deleting pass makes the two agree; the head pass keeps the unbounded form, so
// containment is unchanged at any run length.
const (
	fenceZeroWidthSet    = `[\p{Cf}\p{Mn}\p{Me}\x{00AD}]`
	fenceSepClass        = `[\p{Z}\p{Cc}]`
	fenceMaxSepRun       = 2
	fenceMaxZeroWidthRun = 8
)

// fenceZeroWidthClass is the unbounded form, used by the in-place head pass only.
const fenceZeroWidthClass = fenceZeroWidthSet + `*`

// fenceZeroWidthBounded is the deleting pass's form: same set, capped run.
var fenceZeroWidthBounded = fenceZeroWidthSet + `{0,` + itoa(fenceMaxZeroWidthRun) + `}`

// fenceIgnorableClass is one inter-rune gap of a tag name for the DELETING pass:
// at most fenceMaxZeroWidthRun zero-width runes per position, interleaved with at
// most fenceMaxSepRun visible separators. Both counts feed fenceMaxDeletion.
var fenceIgnorableClass = fenceZeroWidthBounded +
	`(?:` + fenceSepClass + fenceZeroWidthBounded + `){0,` + itoa(fenceMaxSepRun) + `}`

// fenceHeadIgnorableClass is the same gap for the IN-PLACE pass, with the
// separator run unbounded.
//
// The bound on fenceIgnorableClass exists only to cap deletion. The head pass
// deletes nothing, so it carries no bound — which is what closes round 13's
// pad-past gap, where `</文   档数据>` (three spaces, two keystrokes past the cap)
// matched neither pass and reached the model byte-identical.
var fenceHeadIgnorableClass = fenceZeroWidthClass +
	`(?:` + fenceSepClass + fenceZeroWidthClass + `)*`

// fenceGlobalInvisiblePattern is the pre-pass strip. It stays limited to Cf and the
// soft hyphen for the reason above: those have no legitimate rendering role in
// prose, whereas Mn/Me do.
//
// fenceGlobalInvisibleKeep carves out the Cf runes that ARE load-bearing in real
// text. Stripping them globally corrupts the document being summarized, which is a
// faithfulness bug in a product whose whole job is to reproduce that text:
//
//	U+200D ZWJ   — joins family emoji and Devanagari conjuncts
//	U+200C ZWNJ  — ORTHOGRAPHIC in Persian and Devanagari: "می‌روم" and "میروم"
//	               are different words, not different renderings of one word
//	U+200E LRM / U+200F RLM — bidi ordering in mixed Arabic/Hebrew + Latin text;
//	               stripping them silently reorders the visible string
//	U+2060 WORD JOINER — a no-break point; removing it changes line breaking
//
// Carving these out costs no containment, because fenceIgnorableClass (which
// includes ALL of Cf) still neutralizes them INSIDE a tag name: `</\u200c文档数据>`
// and `</文\u200c档数据>` are both caught by the guard patterns themselves. The
// global pass exists only to shrink the search space, not to provide containment.
var fenceGlobalInvisiblePattern = regexp.MustCompile(`[\p{Cf}\x{00AD}]`)

var fenceGlobalInvisibleKeep = map[string]bool{
	"\u200d": true, // ZERO WIDTH JOINER
	"\u200c": true, // ZERO WIDTH NON-JOINER
	"\u200e": true, // LEFT-TO-RIGHT MARK
	"\u200f": true, // RIGHT-TO-LEFT MARK
	"\u2060": true, // WORD JOINER
	// Bidi isolates are the modern replacement for LRM/RLM and do MORE reordering,
	// not less, so the carve-out's own rationale applies to them a fortiori.
	// Stripping them turned "订单 ⁦ABC-123⁩ 已发货" into text the browser reorders.
	"\u2066": true, // LEFT-TO-RIGHT ISOLATE
	"\u2067": true, // RIGHT-TO-LEFT ISOLATE
	"\u2068": true, // FIRST STRONG ISOLATE
	"\u2069": true, // POP DIRECTIONAL ISOLATE
	"\u061c": true, // ARABIC LETTER MARK
}

// fenceControlFolds collapse separators that would otherwise let a tag straddle a
// line/record boundary invisibly.
var fenceControlFolds = []string{
	"\r", " ", "\t", " ", "\x00", " ", "\v", " ", "\f", " ",
	"\u0085", " ", "\u2028", " ", "\u2029", " ",
}

var fenceDelimiterReplacer = strings.NewReplacer(
	append(append([]string{}, fenceDelimiterFolds...), fenceControlFolds...)...,
)

// fenceOpenNeutralized replaces a `<` that would otherwise open a forged fence.
//
// It is one rune, so the head pass can neutralize IN PLACE instead of deleting.
// That single property is what lets the head pass be unbounded (see headPattern),
// and it is chosen from outside the delimiter alphabet: `[` is not a fold source
// for `<` in fenceDelimiterFolds, so neutralizing can never manufacture a new
// opener. It also matches the bracket the full-tag placeholder already uses.
const fenceOpenNeutralized = "["

// fenceMaxRewritePasses caps the fixpoint loop.
//
// The cap is safe ONLY because the head pass converges in a single pass by
// construction: it neutralizes every `<` inside its own match, including the
// boundary rune, so its output cannot contain a `<` adjacent to a tag name for a
// later pass to find. The loop therefore exists for the tag pass's shortening
// rewrites, and 2 iterations suffice today; 8 is headroom, not a tuning knob.
//
// A cap alone would NOT be a fix. Round 13 bounded the prefix to 8 runes while
// leaving the loop bounded by len(s), which made `"<"*N + tagName` take N/9 full
// scans — O(N²), 2.9s at N=8000, and 683ms on the already-shipped <引用数据> path
// reached by ordinary IM message text. Capping iterations there would have traded
// the CPU blowup for a containment hole: the passes that never ran are the ones
// that would have removed the remaining `<` run. Convergence has to be structural
// first; the cap is then just an assertion that it is.
const fenceMaxRewritePasses = 8

// fenceGuard neutralizes forged fence tags for exactly one tag name.
type fenceGuard struct {
	tagName string
	// tagPattern matches a well-formed tag (optional attribute/self-closing tail)
	// and is REWRITTEN TO A SHORTER PLACEHOLDER, so its prefix and in-name
	// separator runs stay bounded — whatever it matches, it deletes.
	tagPattern *regexp.Regexp
	// headPattern matches the tag HEAD alone, with no closing `>` required, and is
	// rewritten IN PLACE (every `<` in the match becomes fenceOpenNeutralized).
	//
	// Because it deletes nothing, it needs no numeric bounds — and that is the
	// point. Rounds 6→7→8 (tail), 10→11→12 (prefix) and 12→13 (separators) each
	// replaced one bound with another, and each time the bound was a number the
	// attacker could pad past: `</文   档数据>` (3 spaces) and `</////////文档数据>`
	// both shipped verbatim under round 13's counts. The bounds existed only to cap
	// how much document text a false positive could erase; a rewrite that erases
	// nothing does not need them. So this pass — the one that actually carries
	// containment — has no attacker-selectable limit anywhere in it.
	headPattern *regexp.Regexp
	// placeholder is 2 runes longer than tagName ("[" + tagName + "]"), matching the
	// 2 delimiter runes of the shortest full tag `<tagName>` exactly.
	placeholder string
	// stripCleanupPattern matches what the head pass LEAVES BEHIND: a neutralized
	// opener followed by the prefix and the tag name. strip removes it to measure
	// emptiness; nothing else uses it, and it never runs on model-bound text.
	//
	// It is anchored on fenceOpenNeutralized rather than on `<` so that it can only
	// ever match this guard's own output, not arbitrary prose: a document that merely
	// mentions 文档数据 keeps it, and only text the head pass already rewrote is removed.
	//
	// The trailing `\p{Zs}*>?` closes round 15's P2-2. When the tag pass declines a tag
	// because its zero-width run exceeds fenceMaxZeroWidthRun, the head pass
	// neutralizes the opener in place and PRESERVES the boundary rune — which for a
	// well-formed tag is its own `>`. Without consuming that, strip returned `">"` for
	// `<文` + 20 combining marks + `档数据>`, a lone delimiter the emptiness gate counted
	// as content: the body cleared 40004 and bought a completion. Only an ADJACENT `>`
	// is consumed, so real text following a neutralized tag still counts as content.
	stripCleanupPattern *regexp.Regexp
}

func newFenceGuard(tagName string) *fenceGuard {
	// Interleave the ignorable class around every rune of the tag name, including a
	// leading position so `<\uFE0F文档数据>` is covered too. Each rune is matched as an
	// ALTERNATION of itself and its NFKC preimages (fenceTagNameFolds), so `</⽂档数据>`
	// is caught without rewriting a legitimate ⽂ anywhere else in the document.
	//
	// Two variants, for the same reason there are two prefix grammars below: the
	// deleting pass gets bounded separator runs (it must not erase much), the
	// in-place pass gets unbounded ones (it erases nothing).
	buildName := func(gap string) string {
		var b strings.Builder
		b.WriteString(gap)
		for _, r := range tagName {
			b.WriteString(fenceTagRuneAlternation(r))
			b.WriteString(gap)
		}
		return b.String()
	}
	n := buildName(fenceIgnorableClass)
	headName := buildName(fenceHeadIgnorableClass)

	// fencePrefix is the noise tolerated between `<` and the tag name. Like the tail
	// boundary, it is NEGATIVE and adjacency-based, and for the same reason: round 8
	// replaced the tail allow-list after `</文档数据">` escaped it, but left this side
	// as the allow-list `[\s\p{Zs}]*/*[\s\p{Zs}]*`. Fuzzing found the mirror bypass —
	// `<"文档数据>`, `<.文档数据>`, `<!文档数据>`, `<:文档数据>` all shipped verbatim.
	//
	// Round 11 shipped `(?:[^>]*[^\p{L}\p{Nd}>])?` and got the direction wrong in the
	// other axis: `[^>]*` crosses arbitrary prose, newlines included, so a single `<`
	// anywhere in a body swallowed everything up to the next mention of the tag name
	// and replaced it with a 4–6 rune placeholder, with no truncation marker.
	//
	// Round 12 restricted the prefix to solidus-terminated word runs and CLAIMED the
	// deletion was thereby bounded to the tag's own syntactic prefix. It was not:
	// fenceDelimNoise was `[^\p{L}\p{Nd}>]*`, unbounded and newline-crossing, so a
	// markdown table (no letters, no digits, no `>`) between a stray `<` and the tag
	// name was still swallowed whole — measured at 4,003 runes lost from a 4,027-rune
	// document, and the same regression on the already-shipped <引用数据> path.
	// Over-neutralizing is the safe direction for injection but NOT for a summarizer:
	// it silently corrupts the very text the product exists to summarize, with no
	// truncation marker to tell anyone it happened.
	//
	// Three rules keep both failure modes closed at once:
	//
	//  1. The prefix carries no bare word run. It may cross a word run ONLY when that
	//     run is terminated by a solidus, i.e. markup shape rather than prose
	//     (`<0/文档数据>`, found by fuzzing in round 10).
	//  2. NEITHER the prefix NOR THE TAIL crosses a LINE BREAK. A fence tag is a token;
	//     a token does not span paragraphs. Round 15: this rule used to be stated over
	//     the prefix alone, and the claim that followed it — "this alone caps the blast
	//     radius at one line" — was FALSE, because the TAIL was `[^>]{0,64}` and `[^>]`
	//     matches `\n`. `当 a < 文档数据 的长度\n第二节：结论很重要\n…\nb > 0` lost two whole
	//     paragraphs, and 100 such units lost 86% of the document with
	//     doc.Truncated=false and no marker. The bound was stated over the wrong half of
	//     the pattern — and the wrong half was the one that deletes.
	//  3. Every unbounded repetition is replaced by an explicit COUNT
	//     (fenceMaxPrefixNoise, fenceMaxPrefixSegments). Deletion is therefore bounded
	//     by a constant this file states, which is what makes the quantitative bound in
	//     TestFenceGuardDeletionIsBounded and FuzzFenceGuard invariant 7 assertable at
	//     all — round 12's claim of boundedness had no number behind it and no test that
	//     could have failed.
	//
	// These counts are NOT a security boundary of the round-6 `{0,64}` kind. Padding
	// past them does not deliver a clean fence to the model; it delivers a tag with
	// visible junk wedged between `<` and the name (`<~~~~~~~~~~/文档数据>`), which no
	// longer reads as the fence — the same residual class as `<文档数据格式说明>`. The
	// bare-head pattern also still fires on the `<`-adjacent form. What the counts do
	// buy is a hard ceiling on how much document text a false positive can erase.
	//
	// Adjacency still decides token identity, which is what keeps prose intact:
	// `<0文档数据` stays prose (`0` continues the name), while `<0/文档数据>` is a tag.
	// Excluding `>` throughout stops a stray `<` reaching across an already-closed tag.
	//
	// The solidus-terminated segments additionally forbid separators, so they match
	// markup shape (`</data/文档数据>`, `<0/文档数据>`) but not prose that merely mentions
	// a path: `比较键 < docs/文档数据` keeps every rune, because the space after `<` cannot
	// be crossed by a segment and `docs` cannot be crossed by the delimiter noise.
	//
	// Residual false positives, bounded and accepted: a `<` separated from the tag
	// name by pure punctuation still neutralizes it, so `x <= 文档数据` loses `<=` and
	// `如果 a < (文档数据)` loses `< (`. These cost at most fenceMaxPrefixNoise runes and
	// are indistinguishable at this layer from `< 文档数据>`, which round 11 established
	// must be caught.
	// Two prefix grammars, because the two passes have different obligations.
	//
	// fencePrefix (BOUNDED) feeds tagPattern, which rewrites to a shorter
	// placeholder. Every rune it matches is a rune deleted from the document, so the
	// counts cap how much a false positive can erase — the round-12 defect where a
	// 200-row markdown table between a stray `<` and the tag name vanished whole.
	//
	// fenceHeadPrefix (UNBOUNDED) feeds headPattern, which rewrites IN PLACE. It
	// deletes nothing, so there is nothing to cap — and no number for an attacker to
	// pad past. Line breaks stay excluded for a reason that is not about deletion: a
	// fence tag is a token, and a token does not span paragraphs.
	const fenceDelimNoise = `[^` + fenceWordClass + `>\r\n\x{2028}\x{2029}]{0,` + fenceMaxPrefixNoiseStr + `}`
	const fencePathNoise = `[^` + fenceWordClass + `>\p{Z}\p{Cc}]{0,` + fenceMaxPrefixNoiseStr + `}`
	const fencePrefix = `(?:` + fencePathNoise + `[` + fenceWordClass + `]{1,` + fenceMaxPrefixNoiseStr + `}/){0,` + fenceMaxPrefixSegmentsStr + `}` + fenceDelimNoise

	const fenceHeadDelimNoise = `[^` + fenceWordClass + `>\r\n\x{2028}\x{2029}]*`
	const fenceHeadPathNoise = `[^` + fenceWordClass + `>\p{Z}\p{Cc}]*`
	const fenceHeadPrefix = `(?:` + fenceHeadPathNoise + `[` + fenceWordClass + `]+/)*` + fenceHeadDelimNoise

	// The optional attribute / self-closing tail, on the DELETING pass.
	//
	// Both halves exclude line breaks, for the reason rule 2 above gives and with the
	// urgency the prefix has: this pattern deletes what it matches. Round 6 wrote the
	// tail as `[\s\p{Zs}/][^>]{0,64}` and BOTH halves match `\n` — `\s` does, and `[^>]`
	// does — so a stray `<` before the tag name and a `>` two paragraphs later deleted
	// everything in between. normalize() has already folded every other control
	// separator to a space, so `\n` is the only line break that can still be here; the
	// rest are excluded for symmetry with fenceDelimNoise, not because they can occur.
	const fenceTailLead = `[\p{Zs}\t\v\f\r/]`
	const fenceTailRest = `[^>\r\n\x{2028}\x{2029}]{0,` + fenceMaxTailStr + `}`
	const fenceTail = `(?:` + fenceTailLead + fenceTailRest + `)?`

	return &fenceGuard{
		tagName: tagName,
		// The optional tail must START with whitespace or a solidus, i.e. real
		// attribute/self-closing syntax, so prose that merely contains the tag name
		// (`<文档数据格式说明>`) is left alone. Anything this declines is still caught by
		// headPattern, so declining costs no containment.
		tagPattern: regexp.MustCompile(`<` + fencePrefix + n + fenceTail + `>`),
		// The boundary condition is deliberately NEGATIVE — "the tag name is not
		// continued by another letter or digit" — rather than an allow-list of
		// delimiters. An allow-list is a bound the attacker picks: round 8 briefly used
		// `[\s\p{Zs}/>]` and `</文档数据">` (one punctuation rune) matched neither pass
		// and shipped verbatim. Since these tag names are CJK, the only thing that makes
		// `<文档数据…` a different TOKEN is a letter/digit continuation.
		//
		// Its in-name gaps use headName (unbounded separators) and its prefix is
		// unbounded, both safe because this pass preserves what it matches: round 13's
		// `</文   档数据>` and `</////////文档数据>` are neutralized at any padding width
		// rather than at two and eight.
		headPattern: regexp.MustCompile(`<` + fenceHeadPrefix + headName + `([^` + fenceWordClass + `]|$)`),
		placeholder: "[" + tagName + "]",
		stripCleanupPattern: regexp.MustCompile(
			regexp.QuoteMeta(fenceOpenNeutralized) + fenceHeadPrefix + headName + `\p{Zs}*>?`),
	}
}

// normalize folds delimiter homoglyphs and strips globally-safe invisibles. Split
// out so neutralize and strip share exactly one normalization path.
func (g *fenceGuard) normalize(s string) string {
	s = fenceDelimiterReplacer.Replace(s)
	return fenceGlobalInvisiblePattern.ReplaceAllStringFunc(s, func(m string) string {
		if fenceGlobalInvisibleKeep[m] {
			return m
		}
		return ""
	})
}

// neutralize replaces forged fence tags with a NON-EMPTY placeholder.
//
// A non-empty placeholder is load-bearing twice over: it keeps the injection
// visible to the model as neutralized text, and it prevents split-token reassembly
// where deleting a tag splices its neighbours into a fresh copy of the same token.
//
// The result is TrimSpace'd. Callers that must preserve leading/trailing layout
// (sanitizeRefBlock, which exists specifically to keep block indentation) use
// neutralizePreservingSpace instead — centralizing the two guards in round 12
// silently imported this trim onto the shipped <引用数据> block path and started
// stripping indentation off quoted code in the agent prompt.
//
// Budget invariant relied on by callers that pre-compute a rune budget: this can
// only ever SHORTEN the text. Pass 1's shortest match `<tagName>` is 2 runes longer
// than tagName and maps to a placeholder exactly 2 runes longer than tagName;
// pass 2's shortest match `<tagName` is 1 rune longer and maps to tagName itself
// with the boundary rune preserved. Ignorables and folded delimiters only ever make
// a match longer while the replacement stays fixed.
//
// Both passes run to a FIXPOINT. Preserving the boundary rune is what keeps prose
// intact, but the boundary rune can itself be a `<`, and then the replacement sits
// next to it as a fresh tag: FuzzFenceGuard found `<文档数据<文档数据`, where one pass
// leaves `文档数据<文档数据` — an intact tag, and a violation of idempotence too. Looping
// is the structural answer rather than another pattern edit: it terminates because
// every rewrite strictly shortens the text (the same invariant the prompt budget
// already depends on), and it makes idempotence hold by construction instead of by
// argument.
func (g *fenceGuard) neutralize(s string) string {
	return strings.TrimSpace(g.neutralizePreservingSpace(s))
}

// neutralizePreservingSpace is neutralize without the surrounding TrimSpace, for
// render sites where leading indentation and trailing blank lines are content.
func (g *fenceGuard) neutralizePreservingSpace(s string) string {
	return g.rewriteToFixpoint(g.normalize(s), g.placeholder)
}

// strip removes fence tags OUTRIGHT. Used only to measure whether a document has
// any real content left; it must not be used on model-bound text, where the
// non-empty placeholder of neutralize is what preserves the injection's visibility.
//
// Round 15: strip is DERIVED from neutralize instead of being a second rewriting
// mode, which is what makes it converge.
//
// The old strip-mode head branch deleted the match while preserving the boundary
// rune `${1}`. For a run of forged heads the boundary rune of match k IS the opening
// `<` of match k+1, and ReplaceAll's matches are non-overlapping, so each pass
// removed only about half of them: convergence took ~log2(k) passes and the
// fenceMaxRewritePasses cap of 8 was REACHED at k>=256, returning intact
// `<文档数据` heads. documentPreviewHasNoContent then saw non-empty output for a body
// of nothing but forged tags, cleared the 40004 gate, and bought a completion.
//
// Deriving strip from neutralize also collapses the mode split that produced
// round 15's P2-1 and P2-2: there is now exactly ONE rewriting path, so a bound or
// an invariant verified for neutralize holds for strip by construction rather than
// by a parallel argument nothing exercised.
//
// Order matters: the full-tag placeholder is removed BEFORE stripCleanupPattern,
// because the placeholder starts with fenceOpenNeutralized and the cleanup pattern
// would otherwise eat its opening bracket and leave the `]`.
func (g *fenceGuard) strip(s string) string {
	out := g.rewriteToFixpoint(g.normalize(s), g.placeholder)
	out = strings.ReplaceAll(out, g.placeholder, "")
	out = g.stripCleanupPattern.ReplaceAllString(out, "")
	return strings.TrimSpace(out)
}

func (g *fenceGuard) rewriteToFixpoint(s, tagReplacement string) string {
	for i := 0; i < fenceMaxRewritePasses; i++ {
		if !strings.ContainsRune(s, '<') {
			return s
		}
		next := g.tagPattern.ReplaceAllString(s, tagReplacement)
		// Head pass runs second, over whatever pass 1 declined, so ordinary well-formed
		// tags still render as the nicer [tagName].
		//
		// It rewrites IN PLACE: every `<` inside the match is replaced by
		// fenceOpenNeutralized and everything else is preserved. Two consequences, both
		// load-bearing:
		//
		//  1. It converges in ONE pass. A `<` run in front of the name is neutralized
		//     whole, not nine runes at a time, so `"<"*N + tagName` is linear instead of
		//     the O(N²) that round 13 shipped (2.9s at N=8000, and 683ms on the
		//     already-shipped <引用数据> path, reached by ordinary IM message text).
		//  2. It deletes nothing, so it needs no bounds, so there is no count for an
		//     attacker to pad past — see headPattern.
		//
		// There is no longer a delete-mode variant of this pass. strip() is derived from
		// this one (see strip), because the delete-mode variant preserved the boundary
		// rune and therefore did NOT converge on overlapping runs — round 15's P1.
		next = g.headPattern.ReplaceAllStringFunc(next, neutralizeFenceOpeners)
		if next == s {
			return s
		}
		s = next
	}
	return s
}

// deletionBudget is the AGGREGATE ceiling for one rewriteToFixpoint call on this
// input: the per-match, per-pass bound (fenceMaxDeletion) summed over the matches
// each pass actually sees.
//
// It exists because round 13's invariant counted matches with ONE pass of the
// patterns and compared the result against a loss accumulated over many — an
// assertion a 10-rune input falsified. Replaying the passes here keeps the
// accounting and the rewriting structurally identical: this function mirrors
// rewriteToFixpoint's loop exactly, so a future edit to one that is not made to the
// other shows up as a failing bound rather than as silence.
//
// Only the tag pass is charged, because it is the only pass that deletes: the head
// pass substitutes one rune for one rune. Round 14 carried a `stripHead` parameter
// here and a comment claiming the invariant was "stated over both" modes; it was
// never called with it, and it would have failed if it had been (~8× over budget),
// because strip's head pass deleted an unbounded match. Round 15 removed that mode
// entirely — strip is now derived from neutralize — so there is one path to charge.
func (g *fenceGuard) deletionBudget(normalized string) int {
	perMatch := fenceMaxDeletion(g.tagName)
	budget := 0
	s := normalized
	for i := 0; i < fenceMaxRewritePasses; i++ {
		if !strings.ContainsRune(s, '<') {
			break
		}
		budget += len(g.tagPattern.FindAllString(s, -1)) * perMatch
		next := g.tagPattern.ReplaceAllString(s, g.placeholder)
		next = g.headPattern.ReplaceAllStringFunc(next, neutralizeFenceOpeners)
		if next == s {
			break
		}
		s = next
	}
	return budget
}

// neutralizeFenceOpeners rewrites one head match in place, replacing every `<` it
// contains with fenceOpenNeutralized and preserving every other rune.
//
// Replacing ALL of them, not just the leading one, is what makes the pass
// idempotent in a single application: the match can contain further `<` runes in
// its prefix or in the preserved boundary rune, and leaving any of them would hand
// the next pass a fresh candidate — the `<文档数据<文档数据` shape fuzzing found in
// round 12.
func neutralizeFenceOpeners(match string) string {
	return strings.ReplaceAll(match, "<", fenceOpenNeutralized)
}
