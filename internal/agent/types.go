package agent

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// 自定义线格式类型：贴 OpenAI chat/completions，刻意不复用 internal/service，
// 以保证 internal/agent 零侵入、零本项目依赖。

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Parameters 直接透传 JSON Schema，用 any 以免绑死结构。
	Parameters any `json:"parameters"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls 仅 assistant 轮次携带；tool_call_id/name 仅 role:"tool" 结果消息携带。
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`

	// RunID and OutputTruncated are persistence-only metadata. They are never
	// sent to the LLM. Binding the degradation to the final assistant message
	// lets the save path judge the exact deliverable the user selected instead
	// of a run-level bit shared by multiple HTTP replay attempts.
	RunID           string `json:"-"`
	OutputTruncated bool   `json:"-"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// AssistantTurn 是单轮 LLM 结果的归一化视图：内容 + 全部工具调用 + 本轮 token 用量。
type AssistantTurn struct {
	Content          string
	ToolCalls        []ToolCall
	Tokens           int // usage.total_tokens; retained for the runner's existing budget semantics.
	CompletionTokens int // usage.completion_tokens; used by diagnostics that report model output size.

	// Truncated reports that this turn's content was cut off by
	// finish_reason=length. It is only ever set on a CONTENT-ONLY turn, i.e. the
	// planner's final user-facing answer: a truncated tool-call turn is rejected
	// outright (its arguments may be cut mid-JSON) and an empty truncated turn is
	// an error, so neither reaches a caller.
	//
	// Carried as a structural field rather than left implicit in the appended
	// prose notice so the runner can record the degradation as a run fact. The
	// prose alone is not enough: it lives inside model-authored text and can be
	// rewritten away downstream.
	Truncated bool
}

// TerminalOutcome is the structured result produced by a terminal tool. The
// visible assistant bubble is deliberately separate from Payload: callers can
// render a short conversational reply while persisting a preview document (or
// workflow metadata) as structured state instead of treating every assistant
// sentence as a saveable summary.
type TerminalOutcome struct {
	VisibleContent string
	ResultType     string
	Payload        json.RawMessage
}

// TerminalHandler validates and accepts one terminal tool invocation. Terminal
// handlers do not run through the ordinary string-returning tool dispatcher:
// a successful outcome ends the Agent loop and is returned to the caller.
type TerminalHandler func(ctx context.Context, args json.RawMessage) (TerminalOutcome, error)

// RunResult is the structured form of a completed Agent run. Terminal is nil
// for legacy profiles that still finish with a free-text assistant response.
type RunResult struct {
	Reply    string
	Terminal *TerminalOutcome
}

// ContextKeyUID is the context key for storing user ID in request context.
type contextKeyUID struct{}

// ContextKeyUID is exported for use by handler to inject uid into context.
var ContextKeyUID = contextKeyUID{}

// ContextKeyRunOwnerID carries the authenticated owner only for runner-level
// bookkeeping such as output-truncation recording. It is intentionally
// distinct from ContextKeyUID: putting the tool-authorization identity on the
// root runner context would let future chat-profile tools inherit data access
// without going through buildRegistryWithUID's explicit allowlist.
type contextKeyRunOwnerID struct{}

var ContextKeyRunOwnerID = contextKeyRunOwnerID{}

// ContextKeySessionID is the context key for storing session ID in request context.
type contextKeySessionID struct{}

// ContextKeySessionID is exported for use by handler to inject session_id into context.
var ContextKeySessionID = contextKeySessionID{}

// ContextKeyRunID carries the SummaryRun id (SS-03) into tool handlers when
// AGENT_SUMMARY_V2_MODE != off. Empty / absent when V2 is off or the request
// has no run — tools then take the pre-SS-05 recompute path. Used by the
// citation pass to freeze/read the run's manifest (SS-05 B1).
type contextKeyRunID struct{}

// ContextKeyRunID is exported for the handler to inject run_id into context.
var ContextKeyRunID = contextKeyRunID{}

// ContextKeyAllowedArchivedChannels carries the archived thread IDs explicitly
// selected by the user for this request. Tool handlers still resolve channel
// membership through GetUserChannels; this value only scopes which archived
// threads may pass the otherwise-active-only discovery filter.
type contextKeyAllowedArchivedChannels struct{}

var ContextKeyAllowedArchivedChannels = contextKeyAllowedArchivedChannels{}

// SelectedArchivedChannelIDs returns the request-scoped archived thread IDs.
// A copy is returned so tool handlers cannot mutate the context value.
func SelectedArchivedChannelIDs(ctx context.Context) []string {
	allowed, _ := ctx.Value(ContextKeyAllowedArchivedChannels).(map[string]bool)
	ids := make([]string, 0, len(allowed))
	for id, ok := range allowed {
		if ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// ChannelScope identifies one channel selected in a trusted application UI.
// ChannelType uses the WuKongIM storage values (1=DM, 2=group, 5=thread).
type ChannelScope struct {
	ChannelID   string
	ChannelType int
	ChannelName string
	SpaceID     string
	IsArchived  bool
}

type contextKeyAllowedChannelScope struct{}

type mutableChannelScope struct {
	mu            sync.RWMutex
	allowed       map[int]map[string]ChannelScope
	discoveryOpen bool
}

func newMutableChannelScope(channels []ChannelScope, discoveryOpen bool, uid string) *mutableChannelScope {
	scope := &mutableChannelScope{
		allowed:       make(map[int]map[string]ChannelScope),
		discoveryOpen: discoveryOpen,
	}
	scope.add(channels, uid)
	return scope
}

func (s *mutableChannelScope) add(channels []ChannelScope, uid string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addLocked(channels, uid)
}

func (s *mutableChannelScope) addLocked(channels []ChannelScope, uid string) {
	for _, channel := range channels {
		if channel.ChannelID == "" || channel.ChannelType == 0 {
			continue
		}
		if uid != "" {
			channel.ChannelID = pipeline.NormalizeDMChannelID(channel.ChannelID, uid, channel.ChannelType)
		}
		byID := s.allowed[channel.ChannelType]
		if byID == nil {
			byID = make(map[string]ChannelScope)
			s.allowed[channel.ChannelType] = byID
		}
		byID[channel.ChannelID] = channel
	}
}

// WithAllowedChannelScope restricts channel-reading tools to the exact set
// already authorised by the application layer. Calling it with an empty slice
// intentionally installs an empty allowlist; absence of the value keeps legacy
// Agent profiles unchanged.
func WithAllowedChannelScope(ctx context.Context, channels []ChannelScope) context.Context {
	uid, _ := ctx.Value(ContextKeyUID).(string)
	return context.WithValue(ctx, contextKeyAllowedChannelScope{}, newMutableChannelScope(channels, false, uid))
}

// WithDiscoverableChannelScope installs an initially-empty allowlist that may
// be expanded only by a trusted discovery tool after it has resolved channel
// membership. list_channels expands it only for an explicit commit_scope=true
// all-visible-channels decision; exploratory listing remains read-only.
func WithDiscoverableChannelScope(ctx context.Context, initial ...[]ChannelScope) context.Context {
	uid, _ := ctx.Value(ContextKeyUID).(string)
	var channels []ChannelScope
	if len(initial) > 0 {
		channels = initial[0]
	}
	return context.WithValue(ctx, contextKeyAllowedChannelScope{}, newMutableChannelScope(channels, true, uid))
}

// AuthorizeDiscoveredChannels adds channels returned by a trusted discovery
// operation to an open request scope. It is a no-op for closed UI scopes.
func AuthorizeDiscoveredChannels(ctx context.Context, channels []pipeline.ChannelInfo) bool {
	scope, ok := ctx.Value(contextKeyAllowedChannelScope{}).(*mutableChannelScope)
	if !ok || scope == nil || !scope.discoveryOpen {
		return false
	}
	uid, _ := ctx.Value(ContextKeyUID).(string)
	grants := make([]ChannelScope, 0, len(channels))
	for _, channel := range channels {
		grants = append(grants, ChannelScope{
			ChannelID:   channel.ChannelID,
			ChannelType: channel.ChannelType,
			ChannelName: channel.ChannelName,
			SpaceID:     channel.SpaceID,
			IsArchived:  channel.IsArchived,
		})
	}
	scope.add(grants, uid)
	return true
}

// AllowedChannelScopes returns a stable copy of the effective scope selected
// by the UI or granted by discovery during this request.
func AllowedChannelScopes(ctx context.Context) []ChannelScope {
	scope, ok := ctx.Value(contextKeyAllowedChannelScope{}).(*mutableChannelScope)
	if !ok || scope == nil {
		return nil
	}
	scope.mu.RLock()
	defer scope.mu.RUnlock()
	channels := make([]ChannelScope, 0)
	for _, byID := range scope.allowed {
		for _, channel := range byID {
			channels = append(channels, channel)
		}
	}
	sort.Slice(channels, func(i, j int) bool {
		if channels[i].ChannelType != channels[j].ChannelType {
			return channels[i].ChannelType < channels[j].ChannelType
		}
		return channels[i].ChannelID < channels[j].ChannelID
	})
	return channels
}

// RestrictDiscoveredChannels keeps discovery results inside a closed UI scope.
// Open discovery and legacy contexts pass through; only explicit workspace
// selections are reduced to their allowlisted channels.
func RestrictDiscoveredChannels(ctx context.Context, channels []pipeline.ChannelInfo) []pipeline.ChannelInfo {
	scope, restricted := ctx.Value(contextKeyAllowedChannelScope{}).(*mutableChannelScope)
	if !restricted || scope == nil || scope.discoveryOpen {
		return channels
	}
	scope.mu.RLock()
	defer scope.mu.RUnlock()
	filtered := make([]pipeline.ChannelInfo, 0, len(channels))
	uid, _ := ctx.Value(ContextKeyUID).(string)
	for _, channel := range channels {
		lookupID := pipeline.NormalizeDMChannelID(channel.ChannelID, uid, channel.ChannelType)
		if _, ok := scope.allowed[channel.ChannelType][lookupID]; ok {
			filtered = append(filtered, channel)
		}
	}
	return filtered
}

// ChannelAllowedByScope reports whether a request-scoped allowlist exists and
// whether the exact channel/type pair belongs to it.
func ChannelAllowedByScope(ctx context.Context, channelID string, channelType int) (restricted, allowed bool) {
	scope, restricted := ctx.Value(contextKeyAllowedChannelScope{}).(*mutableChannelScope)
	if !restricted || scope == nil {
		return false, false
	}
	if uid, _ := ctx.Value(ContextKeyUID).(string); uid != "" {
		channelID = pipeline.NormalizeDMChannelID(channelID, uid, channelType)
	}
	scope.mu.RLock()
	defer scope.mu.RUnlock()
	_, allowed = scope.allowed[channelType][channelID]
	return true, allowed
}

type contextKeyWorkspaceSpaceID struct{}

// WithWorkspaceSpaceID scopes discovery results to the active Octo space.
// Legacy Agent flows omit this value and retain their existing global view.
func WithWorkspaceSpaceID(ctx context.Context, spaceID string) context.Context {
	return context.WithValue(ctx, contextKeyWorkspaceSpaceID{}, spaceID)
}

func WorkspaceSpaceID(ctx context.Context) string {
	spaceID, _ := ctx.Value(contextKeyWorkspaceSpaceID{}).(string)
	return spaceID
}

type allowedTimeRange struct {
	start time.Time
	end   time.Time
}

type contextKeyAllowedTimeRange struct{}

// WithAllowedTimeRange pins all workspace reads to the server-materialized
// time window. Tool arguments remain in the schema for legacy profiles, but a
// workspace caller cannot accidentally expand or shift this boundary.
func WithAllowedTimeRange(ctx context.Context, start, end time.Time) context.Context {
	return context.WithValue(ctx, contextKeyAllowedTimeRange{}, allowedTimeRange{start: start, end: end})
}

// ResolveAllowedTimeRange returns the trusted workspace range when present;
// otherwise it preserves the caller-provided values.
func ResolveAllowedTimeRange(ctx context.Context, requestedStart, requestedEnd time.Time) (time.Time, time.Time) {
	allowed, ok := ctx.Value(contextKeyAllowedTimeRange{}).(allowedTimeRange)
	if !ok || allowed.start.IsZero() || allowed.end.IsZero() {
		return requestedStart, requestedEnd
	}
	return allowed.start, allowed.end
}

// ErrChannelOutsideSelectedScope is returned when an Agent tries to read a
// channel that the trusted application layer did not include in this request's
// selected scope. It is typed so the runner can reject the attempt without
// permanently poisoning the run's completeness verdict.
type ErrChannelOutsideSelectedScope struct {
	ChannelID   string
	ChannelType int
}

func (e *ErrChannelOutsideSelectedScope) Error() string {
	return "channel is outside the selected summary scope"
}
