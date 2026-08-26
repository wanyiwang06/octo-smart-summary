# Unified summary: safe-track implementation boundary

This branch establishes the low-conflict backend foundation for the unified
summary entry without changing the Agent loop contract.

## Implemented now

- `SummaryWorkflowService` owns the existing `POST /summaries` validation,
  normalization and transaction instead of the HTTP Handler.
- Optional `Idempotency-Key` support prevents duplicate task creation and
  duplicate Worker dispatch; same-key/different-request returns `40009`.
- `DeriveSummaryRoute` expresses the deterministic personal/team/preview/
  revision/explanation/clarification routing boundary. It is a tested contract,
  not production enforcement until the Agent adapter is connected.
- Existing SUM-BE1 `SnapshotScope` validation and PR #202 idempotency patterns
  are reused from `main`; old Loop worktree commits are not cherry-picked.

## Compatibility boundary

`CreateFromLegacyHTTP` deliberately preserves the existing endpoint contract,
including its legacy `uid` override and pre-existing authorization behavior.
It must not be called directly by an Agent tool.

The future Agent adapters must provide separate policy-gated entry points that:

1. bind the creator to the authenticated actor;
2. require an idempotency key;
3. authorize every source and participant in the current Space;
4. require a current server-side proposal version before team creation.

The legacy endpoint still uses `pipeline.DefaultTimeRangeDays` (currently 31)
for compatibility. If the product keeps the proposed seven-day Agent default,
the Agent adapter must pass that explicit range instead of changing legacy HTTP
behavior in this branch.

## Deferred until the core Agent PRs stabilize

- `registry.go` / `runner.go` terminal-tool support (`emit_summary_response`);
- `agent_chat.go` JSON, SSE and history contract changes;
- `agent_message` result metadata and session wiring;
- preview/revision-only save enforcement in `agent_summary*`;
- team proposal persistence and confirmation wiring.

The Agent integration should be based on the final versions of #213, #210 and
#215. PR #209's whole-session finalize semantics are not a prerequisite.
