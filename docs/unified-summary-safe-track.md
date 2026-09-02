# Unified summary: safe-track implementation boundary

This branch implements the backend contract for the capability-gated unified
summary workspace, including Agent preview/revision and Workflow execution.

## Implemented now

- `SummaryWorkflowService` owns the existing `POST /summaries` validation,
  normalization and transaction instead of the HTTP Handler.
- Optional `Idempotency-Key` support prevents duplicate task creation and
  duplicate Worker dispatch; same-key/different-request returns `40009`.
- `DeriveSummaryRoute` expresses and enforces the deterministic personal/team/
  preview/revision/explanation/clarification routing boundary.
- Workspace sessions persist authoritative scope, proposal, preview, Workflow,
  replay, lease and History state.
- Agent terminal responses are validated before preview/revision state is
  persisted, and team Workflow creation requires proposal confirmation.
- Existing SUM-BE1 `SnapshotScope` validation and PR #202 idempotency patterns
  are reused from `main`; old Loop worktree commits are not cherry-picked.

## Compatibility boundary

`CreateFromLegacyHTTP` deliberately preserves the existing endpoint contract,
including its legacy `uid` override and pre-existing authorization behavior.
It must not be called directly by an Agent tool.

The workspace Agent adapter provides policy-gated entry points that:

1. bind the creator to the authenticated actor;
2. require an idempotency key;
3. authorize every source and participant in the current Space;
4. require a current server-side proposal version before team creation.

The legacy endpoint still uses `pipeline.DefaultTimeRangeDays` (default 31) for
compatibility. The workspace accepts explicit ranges up to
`MAX_TIME_RANGE_DAYS` (default 90), while its implicit Agent default remains 7
days. Startup fails if the configured maximum is below the legacy default.

## Integrated surfaces

- `registry.go` / `runner.go` terminal-tool support (`emit_summary_response`);
- `agent_chat.go` JSON, SSE and history contracts;
- `agent_message` result metadata and session wiring;
- preview/revision-only save enforcement in `agent_summary*`;
- team proposal persistence, confirmation and idempotent Workflow creation.
