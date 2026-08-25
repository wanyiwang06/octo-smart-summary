package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNormalizeFetchedDocumentSource_ContentAndChunkCaps(t *testing.T) {
	doc := &documentSummarySource{
		Content: strings.Repeat("字", maxDocumentPromptRunes+500),
	}
	// 250 chunks (> maxDocumentChunks), each 1 rune so the total-rune cap does not bite first.
	for i := 0; i < maxDocumentChunks+50; i++ {
		doc.Chunks = append(doc.Chunks, documentSourceChunk{Text: "x"})
	}
	// One oversized chunk to exercise per-chunk truncation (placed within the cap window).
	doc.Chunks[0] = documentSourceChunk{Text: strings.Repeat("超", maxDocumentChunkRunes+100)}

	normalizeFetchedDocumentSource(doc, documentRefReq{DocumentID: "d1", Version: "v2"})

	if n := utf8.RuneCountInString(doc.Content); n > maxDocumentPromptRunes {
		t.Errorf("content not capped: got %d runes, want <= %d", n, maxDocumentPromptRunes)
	}
	if len(doc.Chunks) > maxDocumentChunks {
		t.Errorf("chunk count not capped: got %d, want <= %d", len(doc.Chunks), maxDocumentChunks)
	}
	if n := utf8.RuneCountInString(doc.Chunks[0].Text); n > maxDocumentChunkRunes {
		t.Errorf("oversized chunk not truncated: got %d runes, want <= %d", n, maxDocumentChunkRunes)
	}
	if doc.DocumentID != "d1" {
		t.Errorf("DocumentID not taken from ref: got %q", doc.DocumentID)
	}
	if doc.Version != "v2" {
		t.Errorf("Version fallback failed: got %q", doc.Version)
	}
	if !doc.Truncated {
		t.Error("Truncated flag should be set when caps drop content")
	}
	// Regression guard (round-3 P0): a capped doc must still carry its retained
	// content into the prompt, not just the marker.
	previewBody := buildDocumentPreviewBody(doc)
	if !strings.Contains(previewBody, "[文档内容已按长度上限截断]") {
		t.Error("capped doc should carry the truncation marker in the prompt")
	}
	if utf8.RuneCountInString(previewBody) < maxDocumentChunkRunes {
		t.Errorf("retained ~%d-rune chunk missing from prompt: only %d runes", maxDocumentChunkRunes, utf8.RuneCountInString(previewBody))
	}
}

func TestNormalizeFetchedDocumentSource_BlankChunksDoNotConsumeCap(t *testing.T) {
	doc := &documentSummarySource{}
	for i := 0; i < maxDocumentChunks; i++ {
		doc.Chunks = append(doc.Chunks, documentSourceChunk{Text: "   "}) // blank
	}
	doc.Chunks = append(doc.Chunks, documentSourceChunk{Text: "真正有内容的一段"})

	normalizeFetchedDocumentSource(doc, documentRefReq{DocumentID: "d1"})

	if len(doc.Chunks) != 1 {
		t.Fatalf("expected the one real chunk to survive, got %d chunks", len(doc.Chunks))
	}
	if doc.Chunks[0].Text != "真正有内容的一段" {
		t.Errorf("real chunk dropped; got %q", doc.Chunks[0].Text)
	}
}

// TestNormalizeFetchedDocumentSource_TitlesAreBudgeted pins the round-14 P2: chunk
// titles are RENDERED into the prompt (`### <title>\n`, buildDocumentPreviewBody)
// but were not counted here.
//
// It never overflowed — buildDocumentPreviewBody's own bodyLimit is authoritative
// — but with the configured caps up to 200 × 200 = ~40k runes of title, half the
// budget, entered the prompt uncounted. The effect is silent and in the wrong
// direction: budgetExhausted trips earlier than this loop believes, so real body
// text at the tail of a long, heavily-sectioned document is dropped to pay for
// titles. Titles became model-visible in round 5; the arithmetic did not follow.
func TestNormalizeFetchedDocumentSource_TitlesAreBudgeted(t *testing.T) {
	const (
		titleRunes = 200 // maxDocumentTitleRunes
		bodyRunes  = 400
	)
	doc := &documentSummarySource{}
	for i := 0; i < maxDocumentChunks; i++ {
		doc.Chunks = append(doc.Chunks, documentSourceChunk{
			Title: strings.Repeat("标", titleRunes),
			Text:  strings.Repeat("正", bodyRunes),
		})
	}
	normalizeFetchedDocumentSource(doc, documentRefReq{DocumentID: "d1"})

	// What normalization CLAIMS fits must be what the prompt builder would actually
	// render — body, title, and the "### " + "\n" markup around it.
	rendered := 0
	for _, c := range doc.Chunks {
		rendered += utf8.RuneCountInString(c.Text)
		if c.Title != "" {
			rendered += utf8.RuneCountInString(c.Title) + len("### ") + len("\n")
		}
	}
	if rendered > maxDocumentPromptRunes {
		t.Errorf("normalization kept %d rendered runes, over the %d-rune budget — titles are not being charged",
			rendered, maxDocumentPromptRunes)
	}
	if !doc.Truncated {
		t.Error("a document that exceeded the budget was not flagged Truncated")
	}
	// Sanity: the cut must come from the budget, not from dropping everything.
	if len(doc.Chunks) == 0 || len(doc.Chunks) >= maxDocumentChunks {
		t.Errorf("expected a budget-driven cut, kept %d of %d chunks", len(doc.Chunks), maxDocumentChunks)
	}
}

func TestFetchSummarySource_HTTPBoundary(t *testing.T) {
	var gotToken, gotAuth string
	// Redirect target that WOULD succeed if the client followed the 302. Pointing the
	// Location at an unresolvable host instead (e.g. evil.example) makes the case pass
	// whether or not CheckRedirect exists — following the redirect just fails DNS and
	// still maps to 502 — so deleting the production callback left the suite green.
	var redirectFollowed bool
	followTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectFollowed = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"document_id":"d1","title":"leaked","version":"v","content":"c"}`))
	}))
	defer followTarget.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Token")
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Query().Get("version") {
		case "redirect":
			w.Header().Set("Location", followTarget.URL)
			w.WriteHeader(http.StatusFound) // 302
		case "forbidden":
			w.WriteHeader(http.StatusForbidden) // 403
		case "notfound":
			w.WriteHeader(http.StatusNotFound) // 404
		case "big":
			w.WriteHeader(http.StatusOK)
			// Syntactically VALID JSON larger than the cap. The previous case emitted raw
			// "x"s, which fail to decode at the first byte whether or not the cap exists —
			// removing the LimitReader left the suite green. A well-formed body can only be
			// rejected by the cap actually biting.
			_, _ = w.Write([]byte(`{"document_id":"d1","title":"t","version":"v","content":"`))
			chunk := strings.Repeat("x", 1<<16)
			for i := 0; i < 80; i++ {
				_, _ = w.Write([]byte(chunk))
			}
			_, _ = w.Write([]byte(`"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"document_id":"d1","title":"t","version":"v","content":"c"}`))
		}
	}))
	defer srv.Close()

	// Build the client through the PRODUCTION constructor. Hand-rolling an
	// http.Client here would test this test's own CheckRedirect callback rather than
	// the one newDefaultDocumentSourceClient installs — deleting the production
	// callback then leaves the suite green.
	t.Setenv("DOCUMENT_SUMMARY_SOURCE_API_URL", srv.URL)
	t.Setenv("DOCUMENT_SOURCE_API_URL", "")
	client := newDefaultDocumentSourceClient()
	if client == nil {
		t.Fatal("newDefaultDocumentSourceClient returned nil with the env var set")
	}
	c, ok := client.(*httpDocumentSourceClient)
	if !ok {
		t.Fatalf("unexpected client type %T", client)
	}
	hdr := http.Header{}
	hdr.Set("Token", "tok-123")
	hdr.Set("Authorization", "bf_should_not_forward")

	statusOf := func(version string) int {
		_, err := c.FetchSummarySource(context.Background(), "sp", "u", "d1", version, hdr)
		var se *documentSourceError
		if err != nil && errors.As(err, &se) {
			return se.status
		}
		if err != nil {
			t.Fatalf("unexpected non-documentSourceError: %v", err)
		}
		return 200
	}

	if s := statusOf("redirect"); s != http.StatusBadGateway {
		t.Errorf("302 should map to 502, got %d", s)
	}
	if redirectFollowed {
		t.Error("client followed the 302: CheckRedirect must refuse redirects so the user's Token is never replayed to another host")
	}
	if s := statusOf("forbidden"); s != http.StatusBadRequest {
		t.Errorf("403 should map to 400, got %d", s)
	}
	if s := statusOf("notfound"); s != http.StatusBadRequest {
		t.Errorf("404 should map to 400, got %d", s)
	}
	if s := statusOf("big"); s != http.StatusBadGateway {
		t.Errorf("oversized payload should map to 502, got %d", s)
	}
	if _, err := c.FetchSummarySource(context.Background(), "sp", "u", "d1", "", hdr); err != nil {
		t.Fatalf("happy path failed: %v", err)
	}
	if gotToken != "tok-123" {
		t.Errorf("Token not forwarded: got %q", gotToken)
	}
	if gotAuth != "" {
		t.Errorf("Authorization must not be forwarded, upstream saw %q", gotAuth)
	}
}

func TestFetchSummarySource_RequestsCanonicalDocsPath(t *testing.T) {
	// P2-1: pin the upstream request PATH. The HTTPBoundary fake service never
	// inspects r.URL.Path, so a future edit could silently move the fetch off
	// octo-docs-backend's canonical route. This asserts the exact path (and that the
	// version rides the query string, not the path).
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document_id":"d1","content":"x"}`))
	}))
	defer srv.Close()

	client := &httpDocumentSourceClient{baseURL: srv.URL, client: srv.Client()}
	if _, err := client.FetchSummarySource(context.Background(), "sp", "u", "d1", "v7", http.Header{}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := "/api/v1/docs/d1/summary-source"; gotPath != want {
		t.Errorf("upstream path = %q, want %q", gotPath, want)
	}
	if want := "version=v7"; gotQuery != want {
		t.Errorf("upstream query = %q, want %q (version must ride the query string)", gotQuery, want)
	}
}

func TestValidateDocumentRefs(t *testing.T) {
	if err := validateDocumentRefs([]documentRefReq{{DocumentID: "ok", Version: "v1"}}); err != nil {
		t.Errorf("valid ref rejected: %v", err)
	}
	longID := strings.Repeat("a", maxDocumentIDLen+1)
	if err := validateDocumentRefs([]documentRefReq{{DocumentID: longID}}); err == nil {
		t.Error("expected error for over-long document_id, got nil")
	}
	// url.PathEscape leaves dot-segments intact, so ".." would reach the document
	// service as /api/v1/docs/../summary-source.
	for _, id := range []string{".", ".."} {
		if err := validateDocumentRefs([]documentRefReq{{DocumentID: id}}); err == nil {
			t.Errorf("expected error for dot-segment document_id %q, got nil", id)
		}
	}
	multi := []documentRefReq{{DocumentID: "A", Version: "1"}, {DocumentID: "A", Version: "2"}}
	if err := validateDocumentRefs(multi); err == nil {
		t.Error("expected error for multiple versions of one document, got nil")
	}
}

// TestValidateDocumentRefs_RejectsControlCharacters pins the log-forging fix.
//
// The id reaches operator logs through several paths. Some quote it with %q; the
// 401/403/404 branch of FetchSummarySource builds an ERROR containing it, and
// document_preview.go logs that error with %v — so quoting at the log site does not
// help there. A single embedded newline in a 55-rune id (well under the 64-rune cap)
// was enough to emit a second, entirely attacker-authored line indistinguishable
// from a genuine handler line. Rejecting the input closes every path at once,
// including ones added later.
func TestValidateDocumentRefs_RejectsControlCharacters(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"newline", "x\n[handler] preview fetch source failed doc=\"victim\" ok"},
		{"carriage return", "x\rfoo"},
		{"NUL", "x\x00foo"},
		{"tab", "x\tfoo"},
		{"C1 control", "x\u0085foo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateDocumentRefs([]documentRefReq{{DocumentID: tc.id}}); err == nil {
				t.Errorf("control character in document_id was accepted: %q", tc.id)
			}
		})
	}
	// The same for version, which is interpolated into the upstream query string.
	if err := validateDocumentRefs([]documentRefReq{{DocumentID: "ok", Version: "v\n1"}}); err == nil {
		t.Error("control character in version was accepted")
	}
	// version is validated with the SAME rune class as document_id: invalid UTF-8 /
	// U+FFFD is rejected too, not just control characters (CR-nit uniformity).
	if err := validateDocumentRefs([]documentRefReq{{DocumentID: "ok", Version: "v\xff1"}}); err == nil {
		t.Error("invalid UTF-8 in version was accepted")
	}
	// Ordinary ids with ASCII punctuation must still pass. Note this list is now an
	// ALLOW-LIST test, not a control-character test: `文档-2026` was blessed here one
	// round ago under the old "control characters only" rule and is deliberately gone.
	//
	// Narrowing to ASCII is a client-visible tightening, taken knowingly: a caller
	// passing a non-ASCII document_id now gets 400 instead of a 404 from upstream.
	// octo-docs-backend issues ids as `d_` + 24 hex, so nothing real is rejected, and
	// the by-reference path is the only consumer (the inline path never sends an id
	// upstream). The alternative — permitting non-ASCII because CJK cannot become a
	// separator — keeps a deny-list shaped rule alive for no reachable benefit.
	for _, id := range []string{"doc-123_v2.final", "a:b", "d_70cf5758a2358d30eaa3aa89", "DOC_9"} {
		if err := validateDocumentRefs([]documentRefReq{{DocumentID: id}}); err != nil {
			t.Errorf("legitimate document_id %q rejected: %v", id, err)
		}
	}
}

// TestValidateDocumentRefs_RejectsPercentEncodedSeparators pins the allow-list.
//
// Rejecting literal "/" and "\\" was not enough. url.PathEscape escapes the "%" of a
// percent-encoded separator to "%25", so ONE decode stage is safe — but an ingress
// that decodes twice turns these back into separators, and then the containment
// argument is gone. Two decode stages is a misconfiguration rather than a default,
// which is why this was P2 and not the blocker; the allow-list ends the class for any
// number of stages, because "%" itself is not permitted.
func TestValidateDocumentRefs_RejectsPercentEncodedSeparators(t *testing.T) {
	for _, id := range []string{
		"%2e%2e%2fadmin",
		"%2F..%2Fadmin",
		"%252e%252e%252fadmin",
		"a%2Fb",
		"%00",
		// Not a separator, but the same principle: no percent-encoding survives.
		"a%20b",
	} {
		if err := validateDocumentRefs([]documentRefReq{{DocumentID: id}}); err == nil {
			t.Errorf("document_id %q accepted; a double-decoding ingress could turn it into a separator", id)
		}
	}
}

// TestValidateDocumentRefs_RejectsEmbeddedPathSegments pins the traversal fix.
//
// document_id is interpolated as a SINGLE path segment upstream. The old rule
// compared the whole string to "." / ".." and reasoned that url.PathEscape made
// everything else safe — but %2F is only safe while nothing decodes it before
// normalizing, and the same comment's other half already assumed a normalizing
// intermediary exists. An nginx-style ingress (decode, normalize, route) defeats it.
//
// The shapes below are the reviewer's probe table, measured OPEN against the
// previous head. Each row's third column is what a normalizing backend would have
// been handed.
func TestValidateDocumentRefs_RejectsEmbeddedPathSegments(t *testing.T) {
	for _, tc := range []struct{ id, wouldBecome string }{
		{"a/../../admin", "/api/v1/admin/summary-source"},
		{"a/../../../../etc/passwd", "/etc/passwd/summary-source"},
		{"a/..", "/api/v1/docs/summary-source"},
		{"x/../y", "/api/v1/docs/y/summary-source"},
		{"a/./b", "/api/v1/docs/a/b/summary-source"},
		{"./x", "/api/v1/docs/x/summary-source"},
		{".", "/api/v1/summary-source"},
		{"..", "/api/v1/summary-source"},
		// A separator alone is enough: it need not carry a dot segment to redirect the
		// credentialed request at a different upstream route.
		{"a/b", "/api/v1/docs/a/b/summary-source"},
		{"a\\b", "backslash: normalized to a separator by some intermediaries"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if err := validateDocumentRefs([]documentRefReq{{DocumentID: tc.id}}); err == nil {
				t.Errorf("document_id %q accepted; a normalizing intermediary would serve %s "+
					"with the caller's Token and X-Space-Id", tc.id, tc.wouldBecome)
			}
		})
	}
}

// TestValidateDocumentRefs_AcceptsRealBackendIDs pins the other direction, so the
// containment rule cannot be tightened into rejecting production traffic.
//
// octo-docs-backend's newDocId is "d_" + randomBytes(12).hex, and the
// summary-source route resolves the primary key rather than a slug, so every real
// id is separator-free by construction.
func TestValidateDocumentRefs_AcceptsRealBackendIDs(t *testing.T) {
	for _, id := range []string{
		"d_70cf5758a2358d30eaa3aa89",
		"d_" + strings.Repeat("a", 24),
		"d_0123456789abcdef01234567",
	} {
		if err := validateDocumentRefs([]documentRefReq{{DocumentID: id}}); err != nil {
			t.Errorf("real octo-docs-backend id %q rejected: %v", id, err)
		}
	}
}

// TestFetchSummarySource_OversizedPayloadIsNotAnOutage pins the classification fix.
//
// A well-formed response past the 4 MiB cap used to fail mid-decode under
// io.LimitReader and map to 50202 文档服务暂不可用 — sending an operator to investigate
// a service that is working correctly and merely verbose. It stays in the 502 class
// (it is a contract disagreement, i.e. our bug) but must say what happened.
func TestFetchSummarySource_OversizedPayloadIsNotAnOutage(t *testing.T) {
	big := strings.Repeat("内", maxDocumentSourceResponseBytes/3+4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(documentSummarySource{DocumentID: "d1", Content: big})
	}))
	defer srv.Close()
	client := &httpDocumentSourceClient{baseURL: srv.URL, client: srv.Client()}

	_, err := client.FetchSummarySource(context.Background(), "sp", "u", "d1", "", http.Header{})
	var srcErr *documentSourceError
	if !errors.As(err, &srcErr) {
		t.Fatalf("want a documentSourceError, got %v", err)
	}
	if srcErr.status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", srcErr.status)
	}
	if !strings.Contains(srcErr.message, "exceeds") {
		t.Errorf("message %q should name the size cap rather than claim an outage", srcErr.message)
	}
}

// TestFetchSummarySource_TrailingGarbageRejected applies the request path's
// "exactly one JSON object" discipline to the response path — the one place in this
// diff where it was not applied.
func TestFetchSummarySource_TrailingGarbageRejected(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"second object", `{"document_id":"d","content":"safe"}{"document_id":"d","content":"evil"}`},
		{"trailing junk", `{"document_id":"d","content":"safe"} garbage`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			client := &httpDocumentSourceClient{baseURL: srv.URL, client: srv.Client()}
			if _, err := client.FetchSummarySource(context.Background(), "sp", "u", "d", "", http.Header{}); err == nil {
				t.Error("a response carrying more than one JSON object was accepted")
			}
		})
	}
	// The valid single-object case must still work.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document_id":"d","content":"safe"}`))
	}))
	defer srv.Close()
	client := &httpDocumentSourceClient{baseURL: srv.URL, client: srv.Client()}
	doc, err := client.FetchSummarySource(context.Background(), "sp", "u", "d", "", http.Header{})
	if err != nil || doc == nil || doc.Content != "safe" {
		t.Errorf("valid single-object response rejected: doc=%+v err=%v", doc, err)
	}
}

func TestNormalizeDocumentRefs_DedupTrimSort(t *testing.T) {
	in := []documentRefReq{
		{DocumentID: " B ", Version: "1"},
		{DocumentID: "A", Version: "1"},
		{DocumentID: "A", Version: "1"},  // dup
		{DocumentID: "  ", Version: "x"}, // empty after trim -> dropped
	}
	out := normalizeDocumentRefs(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 refs after dedup/drop, got %d: %+v", len(out), out)
	}
	if out[0].DocumentID != "A" || out[1].DocumentID != "B" {
		t.Errorf("expected sorted [A, B], got [%s, %s]", out[0].DocumentID, out[1].DocumentID)
	}
}

func TestFetchSummarySource_UpstreamStatusClasses(t *testing.T) {
	// The mapping's stated goal is that an infra failure is not misattributed to the
	// document — and its inverse: a credential rejection and upstream throttling must
	// not be reported as a document-service outage either.
	var upstreamStatus int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(upstreamStatus)
	}))
	defer srv.Close()
	client := &httpDocumentSourceClient{baseURL: srv.URL, client: srv.Client()}

	for _, tc := range []struct {
		upstream int
		want     int
		why      string
	}{
		{http.StatusUnauthorized, http.StatusBadRequest, "rejected token is a credential condition, not an outage"},
		{http.StatusForbidden, http.StatusBadRequest, "no permission on this document"},
		{http.StatusNotFound, http.StatusBadRequest, "no such document"},
		{http.StatusTooManyRequests, http.StatusTooManyRequests, "throttling must stay throttling"},
		{http.StatusBadRequest, http.StatusBadGateway, "contract disagreement is our bug, surfaced to operators"},
		{http.StatusConflict, http.StatusBadGateway, "contract disagreement"},
		{http.StatusInternalServerError, http.StatusBadGateway, "genuine outage"},
		{http.StatusServiceUnavailable, http.StatusBadGateway, "genuine outage"},
		{http.StatusGatewayTimeout, http.StatusGatewayTimeout, "timeout preserved"},
	} {
		upstreamStatus = tc.upstream
		_, err := client.FetchSummarySource(context.Background(), "sp", "u", "d1", "", http.Header{})
		var srcErr *documentSourceError
		if !errors.As(err, &srcErr) {
			t.Fatalf("upstream %d: want a documentSourceError, got %v", tc.upstream, err)
		}
		if srcErr.status != tc.want {
			t.Errorf("upstream %d -> %d, want %d (%s)", tc.upstream, srcErr.status, tc.want, tc.why)
		}
	}
}

func TestFetchSummarySource_RetryAfterIsCapturedAndValidated(t *testing.T) {
	// The upstream header is re-emitted into our own response, so it is validated
	// rather than trusted: only the two RFC 9110 forms survive, everything else is
	// dropped instead of forwarded to a client that would have to parse it.
	var retryAfter string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	client := &httpDocumentSourceClient{baseURL: srv.URL, client: srv.Client()}

	nearFuture := time.Now().Add(90 * time.Second).UTC().Truncate(time.Second)
	for _, tc := range []struct {
		upstream string
		want     string
		why      string
	}{
		{"30", "30", "delta-seconds passes through"},
		{"  30  ", "30", "surrounding whitespace trimmed"},
		{"0", "0", "zero is a valid immediate retry"},
		{nearFuture.Format(http.TimeFormat), nearFuture.Format(http.TimeFormat), "HTTP-date within the cap passes through"},
		{"-5", "", "negative delay is nonsense, dropped"},
		{"999999999", "", "beyond the 24h cap, dropped rather than parking the client"},
		// The cap is a property of the instruction, not of its notation: the same
		// "come back in a century" must not pass merely because it is spelled as a date.
		{"Wed, 21 Oct 2099 07:28:00 GMT", "", "far-future HTTP-date is capped like a large delta-seconds"},
		{"Thu, 01 Jan 1970 00:00:00 GMT", "", "past HTTP-date is the mirror of a negative delta-seconds"},
		{"soon", "", "unparseable value dropped"},
		{"", "", "absent upstream header stays absent"},
	} {
		retryAfter = tc.upstream
		_, err := client.FetchSummarySource(context.Background(), "sp", "u", "d1", "", http.Header{})
		var srcErr *documentSourceError
		if !errors.As(err, &srcErr) {
			t.Fatalf("Retry-After %q: want a documentSourceError, got %v", tc.upstream, err)
		}
		if srcErr.retryAfter != tc.want {
			t.Errorf("Retry-After %q -> %q, want %q (%s)", tc.upstream, srcErr.retryAfter, tc.want, tc.why)
		}
	}
}
