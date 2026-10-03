# hive-operator Roadmap

**Last updated**: 2026-10-03 | **Maintainer**: tuna-os maintainers

## Mission

hive-operator turns Hive fleet rotation, healing, credential sharing, release
rollout, usage pacing, and housekeeping into observable Kubernetes controllers.
It replaces silent shell automation with explicit state, events, metrics, and
safe `Observe` to `Shadow` to `Enforce` promotion.

## Current Status

Six controllers exist: `HiveSpoke`, `ModelLadder`, `SharedAuth`, `HiveRelease`,
`UsagePool`, and `HiveHousekeeping`. Rotation, pacing, watchdog, nudge, release,
shared-auth, and housekeeping responsibilities have documented Enforce paths;
every controller still defaults to Shadow for new objects. There is no tagged
release yet.

The promotion evidence and operating contracts live in
[`docs/rotation-promotion.md`](docs/rotation-promotion.md),
[`docs/liveness-promotion.md`](docs/liveness-promotion.md),
[`docs/housekeeping.md`](docs/housekeeping.md), and
[`docs/release.md`](docs/release.md).

### Priorities

| Priority | Item | Tracking | Status |
|---|---|---|---|
| P0 | Make partial rotation mutations recoverable | [#34](https://github.com/tuna-os/hive-operator/issues/34) | ⬜ Not started |
| P0 | Degrade gracefully when a spoke is unreachable | [#31](https://github.com/tuna-os/hive-operator/issues/31) | ⬜ Not started |
| P1 | Make the release reconciler's state machine explicit | [#49](https://github.com/tuna-os/hive-operator/issues/49) | ⬜ Not started |

## Quarterly Goals

### 2026 Q4

**Theme**: Harden the promoted control plane and make failures actionable.

| Goal | Owner | Tracking | Status |
|---|---|---|---|
| Add rollback semantics to multi-step rotation changes | tuna-os maintainers | [#34](https://github.com/tuna-os/hive-operator/issues/34) | ⬜ Not started |
| Keep observation and metrics available during partial fleet outages | tuna-os maintainers | [#31](https://github.com/tuna-os/hive-operator/issues/31) | ⬜ Not started |
| Separate permanent configuration errors from retryable failures | tuna-os maintainers | [#35](https://github.com/tuna-os/hive-operator/issues/35) | ⬜ Not started |
| Publish contributor setup and controller test guidance | tuna-os maintainers | [#55](https://github.com/tuna-os/hive-operator/issues/55) | ⬜ Not started |

### 2027 Q1

Use Q4 reliability evidence to decide whether to cut the first tagged release.
No controller should change its default promotion mode until its shadow diff,
rollback behavior, and alerts have been exercised against the live fleet.

## Technical Debt Backlog

| Item | Issue | Priority | Effort |
|---|---|---|---|
| `HiveReleaseReconciler` spreads its state machine over many methods | [#49](https://github.com/tuna-os/hive-operator/issues/49) | P1 | L |
| `HiveSpokeReconciler` couples planning, liveness, and metrics | [#48](https://github.com/tuna-os/hive-operator/issues/48) | P1 | L |
| Reconciliation lacks transient and permanent error classes | [#35](https://github.com/tuna-os/hive-operator/issues/35) | P1 | M |

## How to Contribute

Start with [DESIGN.md](DESIGN.md) and the controller-specific document linked
above, then comment on the issue you want to own. Run `make test` before opening
a pull request. Changes to promotion behavior must include tests and update the
corresponding operations document.

The [organization contributor guide](https://github.com/tuna-os/.github/blob/main/CONTRIBUTING.md)
describes the shared pull request process and security policy.

## Roadmap Governance

The tuna-os maintainers own this roadmap. Refresh it after controller promotions,
major fleet incidents, tagged releases, and quarter boundaries. A status change
must link to the test, shadow diff, or operational evidence that supports it.
Propose priority changes through a pull request.
