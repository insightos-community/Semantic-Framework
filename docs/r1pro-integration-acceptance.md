# R1Pro real-robot integration acceptance

Integration target: `integration/r1pro-real-robot`. The existing `main` is not a
merge target for this batch. A later main PR requires administrator review.
Nothing in this branch publishes a release, deploys to a robot, or starts motion.

## Classification and traceability

| Type | Issue | Branch | Result |
|---|---|---|---|
| feature | [#16](https://github.com/insightos-community/Semantic-Framework/issues/16) | `feature/r1pro-pilot-map-integration` | Add real robot Pilot, map sources and localization |
| bugfix | [#17](https://github.com/insightos-community/Semantic-Framework/issues/17) | `bugfix/durable-pilot-stop-reconciliation` | Persist stop evidence across Pilot reconciliation |
| bugfix | [#18](https://github.com/insightos-community/Semantic-Framework/issues/18) | `bugfix/robot-skill-input-result-bindings` | Preserve typed Skill inputs and exact result bindings |
| bugfix | [#19](https://github.com/insightos-community/Semantic-Framework/issues/19) | `bugfix/workflow-answer-recovery-lifecycle` | Resume answered robot tasks without replaying completed actions |
| bugfix | [#20](https://github.com/insightos-community/Semantic-Framework/issues/20) | `bugfix/device-event-replay-bandwidth` | Bound device snapshots and replay reconnect events |
| feature | [#21](https://github.com/insightos-community/Semantic-Framework/issues/21) | `feature/kitchen-task-workflow-guidance` | Add grounded kitchen Task and Workflow guidance |
| bugfix | [#22](https://github.com/insightos-community/Semantic-Framework/issues/22) | `bugfix/bounded-workflow-terminal-summary` | Bound terminal summaries while retaining execution evidence |
| chore | [#23](https://github.com/insightos-community/Semantic-Framework/issues/23) | `chore/r1pro-integration-acceptance` | Document integration acceptance, issue traceability and review coverage |

Each issue records cause, purpose, outcome and acceptance commands. Branches are
stacked in the table order so shared API and UI dependencies are retained. PRs
merge into the integration branch in that order; use merge commits to preserve
ancestry and avoid duplicating earlier changes in subsequent PRs.

## Automated acceptance

Run from a clean checkout with the repository toolchain installed:

```sh
bash tools/verify-real-robot-integration.sh
```

The existing PR CI remains authoritative for release packaging. This portable
entry point exercises source tests/builds without credentials or live hardware.
It does not replace physical acceptance. Validation at the reviewed code head:

- `go test -p 4 ./... -race -count=1 -timeout=10m`: passed.
- Server, Pilot and CLI source builds: passed.
- Coverage includes typed input/result bindings, stop evidence, restart and answer recovery, event replay, project/map ownership, SDK cancellation, and bounded terminal summaries.

## Review evidence and limits

[OCR receipt](reviews/2026-10-16-r1pro.json) records the exact product base/head,
per-file decisions, exclusions, findings and fixes. Open-code-review 1.13.0 was
used in delegate mode: the CLI selected files/rules and the host developer agent
reviewed the diffs and reachable context. This is not an independent external
model approval. Default test/documentation exclusions are counted as skipped,
not as reviewed; production reviewable-file coverage is 100%.

Private operational overlays, live-scene scripts, raw conversation archives,
credentials and local deployment assets were not copied into this repository.
They remain in the original workspace snapshot. The separate robot-adapter
checkout has no configured remote and is outside this Framework/Web submission.
Browser rendering and physical task behavior still require a separate deployment
verification; this batch only integrates reviewed source changes.
