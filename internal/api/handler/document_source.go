package handler

// Shared document-source infrastructure.
//
// The client that fetches a document's summarize-ready content from the document
// service, plus the request/response shapes, ref validation, and chunk
// normalization that any document-summarizing handler builds on. Today the sole
// consumer is the ephemeral AI 速览 preview (document_preview.go); it lives in its
// own file so a future persisted document-summary path can reuse it rather than
// redefine these symbols.
//
// There is deliberately NO fence sanitization here — see the comment above
// normalizeFetchedDocumentSource for why the in-band fence, and the rewriting pass
// it forced over user content, were removed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxDocumentSourceResponseBytes = 4 << 20
	maxDocumentIDLen               = 64
	maxDocumentVersionLen          = 128
	maxDocumentTitleRunes          = 200
	maxDocumentChunkRunes          = 12000
	maxDocumentChunks              = 200
	maxDocumentPromptRunes         = 80000
	// maxRetryAfterSeconds caps an echoed upstream Retry-After at 24h. A larger value
	// is either a bug upstream or an attempt to park a client indefinitely; dropping
	// the header is better than forwarding either.
	maxRetryAfterSeconds = 86400
)

var errDocumentSourceNotConfigured = errors.New("document summary source API is not configured")

type documentSourceError struct {
	status  int
	message string
	// retryAfter carries the upstream Retry-After on the 429 path, after
	// sanitizeRetryAfter has re-canonicalized and range-checked it — not verbatim.
	// Empty on every other path, and empty when the upstream value fails that check.
	// Telling a client to back off without telling it for how long makes the status
	// advisory rather than actionable.
	retryAfter string
}

func (e *documentSourceError) Error() string { return e.message }

type documentRefReq struct {
	DocumentID string `json:"document_id"`
	Version    string `json:"version,omitempty"`
}

type documentSummarySource struct {
	DocumentID string `json:"document_id"`
	Title      string `json:"title"`
	Version    string `json:"version"`

	// CONTENT / CHUNKS CONTRACT.
	//
	// This struct is the only definition of the summary-source payload, so this
	// comment is the contract rather than a description of one that lives elsewhere.
	//
	// Content is the WHOLE document flattened to plain text. Chunks, when non-empty,
	// are a segmentation OF that same text and take precedence: buildDocumentPreviewBody
	// renders chunks and ignores Content entirely (and documentPreviewHasNoContent
	// mirrors that precedence). Sending both is therefore redundant, not additive.
	//
	// As of v1 octo-docs-backend always sends `chunks: []` — getSummarySourceHandler
	// returns `content: pmDocToPlainText(pmDoc)` with a literal empty chunk list,
	// commented "the summary service treats content alone as the whole document" — so
	// only the Content path is live today. The XOR is NOT enforced on the wire, and a
	// producer that starts emitting partial chunks alongside a full Content would
	// otherwise degrade this endpoint silently: the uncovered tail of Content would
	// never reach the model, no cap would fire, and the user would receive a
	// confident-looking 速览 of part of the document. normalizeFetchedDocumentSource
	// therefore flags that shape as truncation rather than trusting the contract.
	Content string                `json:"content"`
	Chunks  []documentSourceChunk `json:"chunks,omitempty"`
	// Truncated is set by normalizeFetchedDocumentSource when any size cap
	// (chunk count, per-chunk runes, or total runes) dropped document content,
	// so the prompt builder can surface the truncation marker. Not wire data.
	Truncated bool `json:"-"`
}

type documentSourceChunk struct {
	ChunkID string `json:"chunk_id,omitempty"`
	Page    int    `json:"page,omitempty"`
	Title   string `json:"title,omitempty"`
	Text    string `json:"text"`
}

type documentSourceClient interface {
	FetchSummarySource(ctx context.Context, spaceID, userID, documentID, version string, header http.Header) (*documentSummarySource, error)
}

type httpDocumentSourceClient struct {
	baseURL string
	client  *http.Client
}

func newDefaultDocumentSourceClient() documentSourceClient {
	base := strings.TrimSpace(os.Getenv("DOCUMENT_SUMMARY_SOURCE_API_URL"))
	if base == "" {
		base = strings.TrimSpace(os.Getenv("DOCUMENT_SOURCE_API_URL"))
	}
	if base == "" {
		return nil
	}
	return &httpDocumentSourceClient{
		baseURL: strings.TrimRight(base, "/"),
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *httpDocumentSourceClient) FetchSummarySource(ctx context.Context, spaceID, userID, documentID, version string, header http.Header) (*documentSummarySource, error) {
	if c == nil || c.baseURL == "" {
		return nil, errDocumentSourceNotConfigured
	}
	escapedID := url.PathEscape(documentID)
	u := c.baseURL + "/api/v1/docs/" + escapedID + "/summary-source"
	if version != "" {
		u += "?version=" + url.QueryEscape(version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Space-Id", spaceID)
	req.Header.Set("X-User-Id", userID)
	// Forward only the Token header (authenticated by StrictAuthMiddleware).
	// Authorization belongs to the bot realm (bf_* bearer) and is never
	// validated on this route group, so forwarding it makes the effective
	// principal ambiguous to the document service.
	if v := header.Get("Token"); v != "" {
		req.Header.Set("Token", v)
	}
	if v := header.Get("Accept-Language"); v != "" {
		req.Header.Set("Accept-Language", v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &documentSourceError{status: http.StatusGatewayTimeout, message: "document source API timeout"}
		}
		return nil, &documentSourceError{status: http.StatusBadGateway, message: err.Error()}
	}
	defer resp.Body.Close()
	// 401 joins 403/404 in the RESPONSE class: from the caller's side all three mean
	// "this document is not readable with your credentials", which is actionable, and
	// reporting it as "文档服务暂不可用" told the user to wait for something that will
	// never resolve itself. In the LOG they stay separable: StrictAuthMiddleware has
	// already resolved this same Token to a user id moments earlier, so an upstream
	// 401 more often means the two services disagree about token audience/format — an
	// operator-actionable, all-users condition — while 403/404 is per-document and
	// per-user. Collapsing them in the dashboard would hide a service-wide
	// misconfiguration inside routine permission noise.
	if resp.StatusCode == http.StatusUnauthorized {
		log.Printf("[handler] document source rejected the forwarded Token (upstream 401) doc=%q user=%q space=%q — check token audience/format agreement between services", documentID, userID, spaceID)
		return nil, &documentSourceError{status: http.StatusBadRequest, message: fmt.Sprintf("document %q is not accessible (upstream %d)", documentID, resp.StatusCode)}
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return nil, &documentSourceError{status: http.StatusBadRequest, message: fmt.Sprintf("document %q is not accessible (upstream %d)", documentID, resp.StatusCode)}
	}
	// Upstream throttling is passed through as throttling. Folding it into the 502
	// outage class inflates the document-service failure signal with load events —
	// the same signal the client-disconnect branch in document_preview.go exists to
	// keep honest — and hides the retry semantics from the caller. Retry-After rides
	// along so the backoff is actionable rather than advisory.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &documentSourceError{
			status:     http.StatusTooManyRequests,
			message:    "document source API is rate limiting",
			retryAfter: sanitizeRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &documentSourceError{status: http.StatusBadGateway, message: fmt.Sprintf("document source API redirected with status %d", resp.StatusCode)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Remaining 4xx (400/409/422/…) mean this service and the document service
		// disagree about the contract. That is our bug, not the user's document and not
		// a credential problem, so it stays in the 502 class where an operator will see
		// it; the real upstream status is carried in the message for the log.
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusGatewayTimeout {
			status = http.StatusGatewayTimeout
		}
		return nil, &documentSourceError{status: status, message: fmt.Sprintf("document source API status %d", resp.StatusCode)}
	}
	var out documentSummarySource
	// The cap is enforced with an explicit +1 probe rather than by letting LimitReader
	// truncate: io.LimitReader returns EOF at the boundary, so an oversized body
	// surfaces as a decode error ("invalid or oversized payload", 502 文档服务暂不可用)
	// when the document service is in fact healthy and merely verbose. Reading one byte
	// past the cap distinguishes the two.
	limited := io.LimitReader(resp.Body, maxDocumentSourceResponseBytes+1)
	body, readErr := io.ReadAll(limited)
	if readErr != nil {
		return nil, &documentSourceError{status: http.StatusBadGateway, message: "document source API response could not be read"}
	}
	if len(body) > maxDocumentSourceResponseBytes {
		// A well-formed but oversized payload is a CONTRACT disagreement, not an outage:
		// reporting it as 文档服务暂不可用 sends an operator to look at a service that is fine.
		// It keeps the 502 class (see the remaining-4xx branch above for why our-bug
		// conditions live there) but says what actually happened.
		return nil, &documentSourceError{status: http.StatusBadGateway, message: fmt.Sprintf("document source API payload exceeds %d bytes", maxDocumentSourceResponseBytes)}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&out); err != nil {
		return nil, &documentSourceError{status: http.StatusBadGateway, message: "document source API returned invalid payload"}
	}
	// Same discipline as the request path (document_preview.go): exactly one JSON
	// object, not one object plus whatever follows it. Without this a valid object
	// trailed by a second one — or by junk whose prefix happens to parse — is accepted
	// silently, which is the one place this diff's own "reject, don't silently accept"
	// rule was not applied.
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, &documentSourceError{status: http.StatusBadGateway, message: "document source API response must contain one JSON object"}
	}
	if out.DocumentID == "" {
		out.DocumentID = documentID
	}
	if out.Version == "" {
		out.Version = version
	}
	return &out, nil
}

// sanitizeRetryAfter validates an upstream Retry-After before we echo it into our
// own response header. The value is attacker-influenced in the same sense as any
// upstream header, so it is re-emitted only in the two forms RFC 9110 defines —
// delta-seconds or an HTTP-date — and dropped otherwise. Nothing downstream has to
// reason about header splitting or about a client parsing garbage.
//
// The ceiling applies to BOTH forms. Checking it only on delta-seconds would make
// the cap a formatting detail: the same "come back in a century" instruction just
// has to be written as a date to sail past it, and a past date is the mirror case
// of the negative delta-seconds already rejected.
func sanitizeRetryAfter(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 64 {
		return ""
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 || secs > maxRetryAfterSeconds {
			return ""
		}
		return strconv.Itoa(secs)
	}
	if t, err := http.ParseTime(v); err == nil {
		delta := time.Until(t)
		if delta < 0 || delta > maxRetryAfterSeconds*time.Second {
			return ""
		}
		return t.UTC().Format(http.TimeFormat)
	}
	return ""
}

// documentSourceClient returns the handler's configured document-source client
// (nil when neither DOCUMENT_SUMMARY_SOURCE_API_URL nor the legacy
// DOCUMENT_SOURCE_API_URL fallback is set — see newDefaultDocumentSourceClient).
func (h *AgentSummaryHandler) documentSourceClient() documentSourceClient {
	return h.documentClient
}

func normalizeDocumentRefs(refs []documentRefReq) []documentRefReq {
	seen := map[string]struct{}{}
	out := make([]documentRefReq, 0, len(refs))
	for _, ref := range refs {
		ref.DocumentID = strings.TrimSpace(ref.DocumentID)
		ref.Version = strings.TrimSpace(ref.Version)
		if ref.DocumentID == "" {
			continue
		}
		key := ref.DocumentID + "\x00" + ref.Version
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocumentID == out[j].DocumentID {
			return out[i].Version < out[j].Version
		}
		return out[i].DocumentID < out[j].DocumentID
	})
	return out
}

// isAllowedDocumentIDRune reports whether every rune of id is in the document_id
// allow-list. See the containment comment in validateDocumentRefs for why this is an
// allow-list rather than a set of forbidden characters.
//
// The set is deliberately wider than octo-docs-backend's actual `d_<24 hex>` shape:
// pinning the exact shape here would couple this service to the other side's id
// format and break the moment it adds a prefix or lengthens the random part, for no
// additional containment — every character below is already path-safe and
// encoding-safe. ":" is included because it appears in ids of the form "ns:id".
func isAllowedDocumentIDRune(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

func validateDocumentRefs(refs []documentRefReq) error {
	versionsByDoc := map[string]string{}
	for _, ref := range refs {
		if utf8.RuneCountInString(ref.DocumentID) > maxDocumentIDLen {
			return fmt.Errorf("document_id too long: %q", ref.DocumentID)
		}
		// Control characters are rejected outright rather than quoted at each use site.
		// The id reaches operator logs through several paths — some quoted with %q, some
		// via an error message interpolated with %v — and a single embedded newline in the
		// unquoted path is enough to forge a complete, attacker-authored log line that is
		// indistinguishable from a genuine one. Quoting every site is a rule that has to
		// hold forever; rejecting the input is a rule that holds once. No legitimate
		// document id contains a control character.
		if strings.IndexFunc(ref.DocumentID, forbiddenRefRune) >= 0 {
			return fmt.Errorf("document_id must not contain control characters: %q", ref.DocumentID)
		}
		// Version uses the SAME rune class as document_id (invalid UTF-8 / U+FFFD as
		// well as control characters), so the two caller-supplied fields are not read
		// as deliberately asymmetric. Both reach the prompt via the document envelope.
		if strings.IndexFunc(ref.Version, forbiddenRefRune) >= 0 {
			return fmt.Errorf("document version must not contain control characters: %q", ref.Version)
		}
		// PATH-SEGMENT CONTAINMENT.
		//
		// documentID is interpolated into the upstream path as a single segment
		// (FetchSummarySource: baseURL + "/api/v1/docs/" + url.PathEscape(id) +
		// "/summary-source"). It must therefore BE one segment.
		//
		// The previous rule compared the whole string to "." and ".." and argued that
		// everything else was safe because url.PathEscape percent-encodes it. That
		// argument was self-contradictory: the case it did handle only matters because a
		// normalizing intermediary collapses dot segments, while the case it dismissed
		// assumed no intermediary ever decodes %2F before normalizing. Both cannot hold
		// of the same proxy. nginx-style ingress does exactly that — decode, then
		// normalize, then route — so an embedded dot segment survived:
		//
		//   "a/../../admin"  ->  wire /api/v1/docs/a%2F..%2F..%2Fadmin/summary-source
		//                    ->  backend sees /api/v1/admin/summary-source
		//
		// reached with the caller's forwarded Token, the middleware-resolved X-User-Id,
		// and the caller-supplied X-Space-Id. Rows like "x/../y" were silent id
		// substitution; "a/../../../../etc/passwd" left the base path entirely.
		//
		// So the rule is containment, not enumeration: a segment separator must never be
		// EMBEDDABLE in the first place, which makes dot segments inexpressible rather
		// than individually forbidden. octo-docs-backend issues ids as `d_<24 hex>`
		// (newDocId = "d_" + randomBytes(12).hex) and the summary-source route resolves
		// the primary key, never a slug, so no legitimate id contains a separator. If
		// that ever changes, the id must move to the query string — do NOT relax this
		// back into a list of forbidden shapes.
		// The rule is an ALLOW-LIST, not a deny-list of separators, because a deny-list
		// only closes the shapes that are separators *at validation time*. Rejecting
		// literal "/" and "\\" left "%2e%2e%2fadmin" and "%2F..%2Fadmin" accepted:
		// url.PathEscape correctly escapes the "%" to "%25", so one decode is safe, but an
		// ingress that decodes twice turns them back into separators and the containment
		// argument above leaks. Two decode stages is a misconfiguration rather than a
		// default, which is why this was not the blocker — but a character that BECOMES a
		// separator one decode later is the same class, and no deny-list ends the class.
		//
		// Permitting only these characters ends it for any number of decode stages: none
		// of them is a separator, and none of them can encode one. "%" is deliberately
		// absent, so no percent-encoding of any depth survives validation.
		if !isAllowedDocumentIDRune(ref.DocumentID) {
			return fmt.Errorf("document_id must match [A-Za-z0-9_.:-]+: %q", ref.DocumentID)
		}
		// Belt-and-braces for the separator-free forms (".", ".."), which pass the
		// allow-list ("." is permitted inside ids such as "doc-123_v2.final") but are
		// still dot segments on their own.
		if ref.DocumentID == "." || ref.DocumentID == ".." {
			return fmt.Errorf("document_id must not be a path segment: %q", ref.DocumentID)
		}
		if utf8.RuneCountInString(ref.Version) > maxDocumentVersionLen {
			return fmt.Errorf("document version too long: %q", ref.Version)
		}
		if existing, ok := versionsByDoc[ref.DocumentID]; ok && existing != ref.Version {
			return fmt.Errorf("multiple versions of one document are not supported: %q", ref.DocumentID)
		}
		versionsByDoc[ref.DocumentID] = ref.Version
	}
	return nil
}

// NOTE ON PROMPT INJECTION CONTAINMENT
//
// This endpoint does NOT wrap untrusted document text in an in-band fence such as
// <文档数据>…</文档数据>. Untrusted text is carried in its OWN chat message (see
// buildDocumentPreviewBody and the message assembly in document_preview.go), so
// there is no in-band delimiter for a document to forge and therefore nothing to
// sanitize away.
//
// This is deliberate and it is the reason no fence guard lives here. An in-band
// fence forces a sanitizer over the very text the endpoint exists to summarize, and
// that sanitizer is a rewriting pass on user content: every bound it draws is either
// too loose (a forged closing tag reaches the model) or too tight (real document
// prose is silently deleted with no truncation marker). Over-neutralizing is safe
// for injection and actively wrong for a summarizer. Moving the boundary from an
// in-band string to the message envelope removes the whole class instead of tuning it.
//
// Consequence for this file: document text is only ever budgeted (rune caps, chunk
// caps, truncation marker) and never pattern-rewritten.

// documentChunkTitlePrefix is the markup buildDocumentPreviewBody renders before
// a chunk title. It lives here rather than as a literal at the render site because
// normalizeFetchedDocumentSource has to charge the same runes to the prompt budget
// — the two drifting apart is exactly the round-14 P2 (titles model-visible since
// round 5, uncounted ever since).
const documentChunkTitlePrefix = "### "

// forbiddenRefRune is the single rune class rejected in caller-supplied document
// refs (document_id AND version) and stripped from upstream-supplied version: it
// covers invalid UTF-8 / U+FFFD and every control character. One closure at every
// site keeps the fields uniform so an asymmetry is never read as intentional.
func forbiddenRefRune(r rune) bool { return r == utf8.RuneError || unicode.IsControl(r) }

// stripForbiddenRefRunes drops forbiddenRefRune runes. Used on the upstream version
// (which is data, not a request, so it is sanitized rather than rejected).
func stripForbiddenRefRunes(s string) string {
	return strings.Map(func(r rune) rune {
		if forbiddenRefRune(r) {
			return -1
		}
		return r
	}, s)
}

func normalizeFetchedDocumentSource(doc *documentSummarySource, ref documentRefReq) {
	doc.DocumentID = ref.DocumentID
	doc.Version = truncateRunes(strings.TrimSpace(stripForbiddenRefRunes(doc.Version)), maxDocumentVersionLen)
	if doc.Version == "" {
		doc.Version = ref.Version
	}
	doc.Title = truncateRunes(strings.TrimSpace(doc.Title), maxDocumentTitleRunes)

	rawContent := strings.TrimSpace(doc.Content)
	doc.Content = truncateRunes(rawContent, maxDocumentPromptRunes)
	// The content field is only rendered when no chunks survive (see
	// buildDocumentPreviewBody). Defer flagging its truncation until we know that,
	// so the marker never claims a cut the model never saw.
	contentTruncated := utf8.RuneCountInString(rawContent) > maxDocumentPromptRunes

	normalized := make([]documentSourceChunk, 0, len(doc.Chunks))
	total := 0
	kept := 0
	for _, chunk := range doc.Chunks {
		// ChunkID/Page are carried for the wire contract only — nothing downstream reads
		// them, so they are deliberately not normalized. Title IS rendered into the
		// prompt (see buildDocumentPreviewBody), so it is clamped like any other
		// untrusted, model-visible string.
		chunk.Title = truncateRunes(strings.TrimSpace(chunk.Title), maxDocumentTitleRunes)
		trimmed := strings.TrimSpace(chunk.Text)
		if trimmed == "" {
			// Blank chunks are dropped and must NOT consume the count cap.
			continue
		}
		if kept >= maxDocumentChunks {
			doc.Truncated = true
			break
		}
		if utf8.RuneCountInString(trimmed) > maxDocumentChunkRunes {
			doc.Truncated = true
		}
		chunk.Text = truncateRunes(trimmed, maxDocumentChunkRunes)
		// Titles are rendered into the prompt as `### <title>\n`
		// (buildDocumentPreviewBody), so they must be charged to the same budget as
		// the body. They became model-visible in round 5 and the arithmetic here did
		// not follow: with the configured caps that is up to 200 chunks × 200 runes =
		// ~40k runes entering the prompt uncounted — half the budget. It never
		// overflowed, because buildDocumentPreviewBody's own bodyLimit is
		// authoritative, but it made budgetExhausted trip earlier than this loop
		// believed, so real body text at the tail of a long, heavily-sectioned document
		// was dropped to pay for titles this loop thought were free.
		//
		// The +len() is the rendered markup itself: the "### " prefix and the trailing
		// newline that buildDocumentPreviewBody writes around every non-empty title.
		cost := utf8.RuneCountInString(chunk.Text)
		titleCost := 0
		if chunk.Title != "" {
			titleCost = utf8.RuneCountInString(chunk.Title) + len(documentChunkTitlePrefix) + len("\n")
		}
		total += cost + titleCost
		if total > maxDocumentPromptRunes {
			// The title is already committed at this point (it renders before the body),
			// so it is charged first and the body gets whatever is left. Charging only
			// the body here is what let the total overrun by the title's own runes.
			remaining := maxDocumentPromptRunes - (total - cost)
			doc.Truncated = true
			if remaining <= 0 {
				break
			}
			chunk.Text = truncateRunes(chunk.Text, remaining)
			normalized = append(normalized, chunk)
			break
		}
		normalized = append(normalized, chunk)
		kept++
	}
	doc.Chunks = normalized
	// contentTruncated is the ONLY signal that the source document was larger than the
	// rune budget, and it is computed from rawContent before doc.Content is cut down.
	// It used to be discarded whenever chunks survived, on the reasoning that the
	// coexistence guard below would cover that case — but the guard compared against the
	// ALREADY-TRUNCATED doc.Content, so an oversized source with chunks segmenting only
	// its leading portion passed both checks and disclosed nothing. Oversize is
	// oversize regardless of which field renders; flagging it unconditionally is what
	// makes the two checks independent rather than mutually load-bearing.
	if contentTruncated {
		doc.Truncated = true
	}
	// COEXISTENCE GUARD — see the Content/Chunks contract on the struct.
	//
	// When chunks survive, buildDocumentPreviewBody renders them and drops Content on
	// the floor. If Content carries text the chunks do not, that text never reaches the
	// model, and because every value can sit far below every cap, none of the size
	// checks above fire: Truncated stays false, no marker is emitted, and the user is
	// billed for a complete-looking 速览 of part of their document. That is the same
	// silent-content-loss shape this endpoint spent its whole review history removing,
	// re-entering through field precedence instead of through a rewriting pass.
	//
	// We deliberately do NOT try to merge the two. A payload that violates the
	// documented contract has no defined semantics — appending Content after the chunks
	// would duplicate the whole document in the common redundant case — so the honest
	// response is to tell the user the source was incomplete rather than to guess.
	//
	// Coverage is judged against rawContent, NOT doc.Content. Comparing against the
	// post-cap value is what let the previous round ship a guard with a hole in exactly
	// the shape it was written to stop: chunks that cover only the retained 80k prefix
	// "cover" a Content that has itself been shortened to that prefix, so the guard went
	// quiet while more than half the document was dropped. Measured at the previous
	// head: a 160,000-rune source rendered 72,208 runes with Truncated=false and no
	// marker.
	//
	// Note for anyone pruning this later: given the unconditional contentTruncated
	// check above, using rawContent here is provably redundant — the two values differ
	// only when content exceeded the cap, and that case is already flagged. Mutation
	// testing confirms it: reverting either line alone keeps the suite green, only
	// reverting both reproduces the defect. It is kept because the guard's correctness
	// should not depend on a separate check three lines up continuing to exist, and
	// because comparing against a value this function itself shortened is the kind of
	// subtlety that produced this round's finding. Redundant, deliberately.
	if len(normalized) > 0 && rawContent != "" && !chunksCoverContent(normalized, rawContent) {
		doc.Truncated = true
	}
}

// chunksCoverContent reports whether the chunk texts already carry everything in
// content, so the coexistence guard can stay silent for a producer that merely sends
// a redundant (fully segmented) payload and only fire when text would actually be
// lost.
//
// Comparison ignores all whitespace: chunking legitimately changes how the text is
// broken up, and a marker raised over re-flowed newlines would be a false alarm — the
// one failure mode a truncation marker cannot afford, since it trains users to ignore
// it. Both sides are already bounded by maxDocumentPromptRunes before this runs, and
// strings.Contains is Rabin-Karp on inputs this size, so this stays linear.
func chunksCoverContent(chunks []documentSourceChunk, content string) bool {
	dropSpace := func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}
	wanted := strings.Map(dropSpace, content)
	if wanted == "" {
		return true
	}
	var joined strings.Builder
	for _, chunk := range chunks {
		joined.WriteString(strings.Map(dropSpace, chunk.Text))
	}
	return strings.Contains(joined.String(), wanted)
}

// documentPreviewHasNoContent reports whether a normalized document has anything
// left to summarize, so a body that is only whitespace does not bill a completion.
//
// Emptiness is plain TrimSpace. With untrusted text in its own message there is no
// fence to discount, so the gate and the prompt builder measure the SAME text —
// they cannot disagree about what counts as content. (While an in-band fence
// existed, the gate had to strip tags before measuring, and "strip" and "render"
// were two different views of one document: a body that looked empty to the gate
// still reached the model, and prose the gate deleted was billed but never seen.)
//
// The chunks-else-content precedence mirrors buildDocumentPreviewBody exactly: the
// builder renders doc.Content ONLY when there are no chunks. Judging the union
// instead would let a doc with blank chunks plus real content clear this gate and
// then bill a completion on a body the model never receives.
func documentPreviewHasNoContent(doc *documentSummarySource) bool {
	if len(doc.Chunks) > 0 {
		for _, chunk := range doc.Chunks {
			if strings.TrimSpace(chunk.Text) != "" {
				return false
			}
		}
		return true
	}
	return strings.TrimSpace(doc.Content) == ""
}
