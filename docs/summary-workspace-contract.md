# Summary workspace contract v1

The summary workbench is available when `GET /summary-workbench/capabilities`
returns `enabled: true` and `contract_version: "1"`.

## Direct team workflow

- `direct_team_workflow: true` means the client may send
  `action: "start_team_workflow"` instead of showing a confirmation card.
- This capability currently ships together with the v1 workbench; it is not a
  separate rollout flag.
- The server accepts the action only for a generate intent with a valid team
  scope and requirement. Source, participant, and reference permissions are
  revalidated before the workflow is created.
- Reusing the same `request_id` is idempotent and does not dispatch a second
  workflow.

## Effective scope

The session's `scope_json` and `scope_hash` are the authoritative effective
scope. When the server resolves a default, inferred source, or explicit
natural-language time-range change, the completed turn stores that resolved
scope without changing `scope_version`. The response returns the same scope in
`state.summary_context`; the client must use it for the next turn.

A pending team proposal also stores its resolved `time_range`. Confirmation
uses that stored value, so the displayed range and the dispatched workflow
window cannot diverge.

## History preview field

History messages for `agent_preview` and `agent_revision` may include an
additive `preview` object. It contains the immutable content and version
metadata for that historical message. Only the artifact referenced by current
state exposes actions; older previews are read-only. Clients that ignore
unknown fields remain compatible with contract v1.
