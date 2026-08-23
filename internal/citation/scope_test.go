package citation

import "testing"

// The rules this package exists to hold ONCE. Every case below is one the repo
// has already paid for: the R11 Q5 list (handler.stripUnresolvedCitationMarkers)
// and the R4 blocking-1 list (worker.remapFinalizeCitations).
func TestRewriteMarkers_PreservesNonMarkerBrackets(t *testing.T) {
	// A rewriter that would destroy everything it is offered, so anything that
	// survives survived because the SCOPING refused to offer it.
	destroy := func(string) (string, bool) { return "<GONE>", true }

	cases := []struct {
		name string
		in   string
	}{
		{"fenced code block", "before\n```go\narr[0] = x\n```\nafter"},
		{"fenced with indent", "before\n  ```\nitems[12] = y\n  ```\nafter"},
		{"markdown inline link", "see [1](https://example.com) here"},
		{"reference-style link", "see [1][docs] for details"},
		{"numeric reference definition", "[1]: https://example.com/doc"},
		{"footnote definition", "[^1]: footnote text"},
		{"unterminated bracket", "an open [3 bracket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RewriteMarkers(tc.in, destroy); got != tc.in {
				t.Errorf("scoping let a non-marker through:\n in  = %q\n out = %q", tc.in, got)
			}
		})
	}
}

// The caller keeps the semantics: a token it declines is byte-identical, a token
// it accepts is spliced. This is what lets the handler (marker set) and the
// worker (pool ordinal) share one syntax without sharing a definition of
// "marker".
func TestRewriteMarkers_CallerDecidesPerToken(t *testing.T) {
	in := "结论 [1],待办共 [3] 项,标准 [2020]"
	got := RewriteMarkers(in, func(token string) (string, bool) {
		if token == "1" {
			return "[9]", true
		}
		return "", false
	})
	want := "结论 [9],待办共 [3] 项,标准 [2020]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRewriteMarkers_AdjacentNumericMarkersAreNotMistakenForReferenceLink(t *testing.T) {
	got := RewriteMarkers("结论 [1][2] 成立", func(token string) (string, bool) {
		return "[R" + token + "]", true
	})
	if want := "结论 [R1][R2] 成立"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRewriteMarkers_NumericReferenceLinkWithDefinitionIsPreserved(t *testing.T) {
	in := "see [1][2] for details\n\n[2]: https://example.com/doc"
	got := RewriteMarkers(in, func(string) (string, bool) { return "<GONE>", true })
	if got != in {
		t.Fatalf("numeric reference link with a real definition changed:\n in  = %q\n out = %q", in, got)
	}
}

func TestRewriteMarkers_InvalidDefinitionsDoNotHideAdjacentMarkers(t *testing.T) {
	rewrite := func(token string) (string, bool) { return "[R" + token + "]", true }
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty definition",
			in:   "adjacent [1][2]\n[2]:",
			want: "adjacent [R1][R2]\n[R2]:",
		},
		{
			name: "definition inside fence",
			in:   "adjacent [1][2]\n```\n[2]: https://example.com/doc\n```",
			want: "adjacent [R1][R2]\n```\n[2]: https://example.com/doc\n```",
		},
		{
			name: "four-space indented line",
			in:   "adjacent [1][2]\n    [2]: https://example.com/doc",
			want: "adjacent [R1][R2]\n    [R2]: https://example.com/doc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RewriteMarkers(tc.in, rewrite); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRewriteMarkers_ReferenceDefinitionMustBeAtLineStart(t *testing.T) {
	in := "[1]: https://example.com/one\n   [2]: https://example.com/two\n根据 [3]: 该结论成立\n根据 [4]：该结论成立"
	got := RewriteMarkers(in, func(token string) (string, bool) {
		return "[R" + token + "]", true
	})
	want := "[1]: https://example.com/one\n   [2]: https://example.com/two\n根据 [R3]: 该结论成立\n根据 [R4]：该结论成立"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRewriteMarkers_UnterminatedSecondLabelDoesNotHideFirstMarker(t *testing.T) {
	got := RewriteMarkers("见 [1][ 未闭合", func(token string) (string, bool) {
		return "[R" + token + "]", true
	})
	if want := "见 [R1][ 未闭合"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Deletion is available to a caller that wants it, and does not disturb
// surrounding bytes (the handler pins its spacing exactly).
func TestRewriteMarkers_DeletionLeavesSpacingToTheCaller(t *testing.T) {
	got := RewriteMarkers("结论 [1] 与 [2] 如下", func(token string) (string, bool) {
		return "", token == "1" || token == "2"
	})
	if want := "结论  与  如下"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRewriteMarkers_NilRewriterIsIdentity(t *testing.T) {
	in := "结论 [1]"
	if got := RewriteMarkers(in, nil); got != in {
		t.Fatalf("got %q, want %q", got, in)
	}
}

// Fence state is tracked across lines, so an unbalanced fence does not spill
// into a rewrite of the rest of the document.
func TestRewriteMarkers_FenceTogglesPerLine(t *testing.T) {
	in := "a [1]\n```\nb [2]\n```\nc [3]"
	got := RewriteMarkers(in, func(string) (string, bool) { return "[X]", true })
	want := "a [X]\n```\nb [2]\n```\nc [X]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
