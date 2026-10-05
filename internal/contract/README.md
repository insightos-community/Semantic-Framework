# Agent contract boundary

This branch extends the existing Task intent boundary repair and validates
Agent output at five interaction points: Leader task publication, Task-to-SubTask
planning, Skill call selection, Skill checkpoint decisions, and failure recovery
decisions. It does not add Trace, prefetch, physical recovery strategies, or
automatic replay of completed robot actions.

## Existing boundaries retained

- Leader tool parameters: the registered tool JSON Schema and security middleware.
- Task → SubTask: strict decoder, approved installed Skill/version checks, intent
  boundary checks, and the existing bounded same-Run correction loop.
- Skill input: `skill.validate_input` executes the installed Pydantic validator
  before creating a Robot Execution. Custom Python validators remain authoritative.
  Existing default normalization, request identity, locks and approval are unchanged.

## New input-contract plumbing

1. Worker initialization already exports `input_model.model_json_schema()`.
2. Pilot retains this result and exposes read-only `skill.describe_input`.
3. Server requires an online bound Pilot, an enabled installed exact version and
   matching returned identity. The contract must compile without external files/URLs.
4. Robot Task execution injects only the selected Skill input schema before the
   first model exchange; direct `robot.get(skill_name)` exposes the same contract.
5. Invalid input returns `ROBOT_SKILL_INPUT_INVALID`, not a physical failure or an
   automatically replayable error. The existing correction path carries the actual
   last tool error. Valid output needs no extra model review call.

`input_schema` describes the nested `robot.run.input`, not the tool envelope or
Task intent. It supplies structure, not permission or live-state evidence. It must
not be reconstructed from examples, model names or the latest Registry package.
Missing contracts fail explicitly rather than falling back to guessed fields.

No cache is introduced initially: discovery costs a short-lived read-only Worker
startup and model prompt bytes. Measure this cost before adding caching; any cache
must account for installed package identity and invalidation, not just Skill name.
Server and Pilot must be deployed together. Python SDK changes are not required.

## Correction and limits

Invalid model output may be fed back in the same Run, with at most two
corrections in this candidate and a further limit from Agent `max_turns`.
Accepted plan submissions are bound to their Run and cannot be resubmitted.
Correction exhaustion stops safely; valid input does not guarantee physical
grasp success. The correction limit is not yet independently configurable.

The developer-facing interface standard and remaining contract-owner questions
are in `semantic-docs/docs/developer/reference/api/agent-interface-contract.md`
on branch `docs/agent-interface-contract`.

## Verification

- `go test ./...` (the external Python tests are opt-in).
- Set `SEMANTIC_ROBOT_SKILL_PYTHON` to the bundled interpreter and
  `SEMANTIC_ROBOT_SKILLS_DIR` to the installed SDK/Skill source root, then run
  `go test ./internal/pilot -run TestInstalledInputContractAndPydanticPreflight -v`.
- Run live-model simulation separately. Controlled injected faults are diagnostics,
  not natural Bug occurrences or unbiased performance samples. Passing structural
  tests does not prove physical grasp reliability or unchanged end-to-end latency.

Future interfaces should declare authoritative types, field semantics, error
protocols and custom validators. The first three interaction points have
controlled live-model fault injection; checkpoint and recovery have read-only
decision-boundary tests, not physical recovery end-to-end proof.

## Rebase acceptance and deployment

The 2026-09-17 candidate was validated on `develop e50fb3c` with product commit
`f5452df`. See the [rebase acceptance record](../../changelog/v0.5.0/robot-planning-holding-boundary.md)
for component, race, live-model and fault-matrix results and their limits.
Deploy the matching Server and Pilot builds together. Migration 34 adds the
per-Run Proposal submission identity table; back up the database before upgrading.
