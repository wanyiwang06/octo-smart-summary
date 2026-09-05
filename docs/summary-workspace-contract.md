# Summary workspace contract v2

The summary workbench is available when `GET /summary-workbench/capabilities`
returns `enabled: true` and `contract_version: "2"`.

## Direct team workflow

- `direct_team_workflow: true` means the client may send
  `action: "start_team_workflow"` instead of showing a confirmation card.
- This capability currently ships together with the v1 workbench; it is not a
  separate rollout flag.
- The explicit action is treated as the user's execution intent. The server
  still requires a valid team scope and requirement, and revalidates source,
  participant, and reference permissions before creating the workflow.
- Reusing the same `request_id` is idempotent and does not dispatch a second
  workflow.

## Effective scope

The session's `scope_json` and `scope_hash` are the authoritative effective
scope. When the server resolves a default, inferred source, or explicit
natural-language source/time-range change, the completed turn stores that
resolved scope without changing `scope_version`. The response returns the same
scope in `state.summary_context`; the client must use it for the next turn and
must replace its local chat chips with the returned `selected_channels`.

Conversational source replacement/extension is two-phase. The Agent first
resolves candidate chats through server-authorised discovery, then calls
`set_summary_scope` with one final `keep`, `replace`, or `extend` decision. The
server accepts only discovered channels and applies contract limits, actor
membership, and team-scope checks before atomically replacing the stored scope.
Ordinary edits, questions, negations, and historical references do not call the
scope tool and therefore keep the picker scope. A source-changing turn cannot
directly dispatch a Workflow; the trusted start action may run after the
resolved scope is returned.

`time_range.source` records whether the current range came from the picker,
the server default, or a conversational instruction. Explicit conversational
ranges, including non-preset ranges such as three days or two weeks, are parsed
by the Agent and declared through `set_summary_scope` before message retrieval.
Incidental, questioned, negated, complained-about, or historical mentions do
not replace the current range.

A pending team proposal also stores its resolved `time_range`. Confirmation
uses that stored value, so the displayed range and the dispatched workflow
window cannot diverge.

## History preview field

History messages for `agent_preview` and `agent_revision` may include an
additive `preview` object. It contains the immutable content and version
metadata for that historical message. Only the artifact referenced by current
state exposes actions; older previews are read-only. Clients that ignore
unknown fields remain compatible with contract v1.
