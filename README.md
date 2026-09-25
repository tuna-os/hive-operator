# hive-operator

A Kubernetes operator for the tuna-os Hive fleet: the rotation, healing,
credential-sharing and pacing that currently run as ~12 shell CronJobs, modelled
as CRDs and controllers, with Prometheus metrics and a fleet dashboard.

**Status: early.** Three controllers exist: `HiveSpoke`, `ModelLadder`, and
`SharedAuth`. All default to Shadow — `HiveSpoke`'s rotation planner runs
alongside the legacy CronJobs rather than replacing them. Nothing has been
cut over yet. Rotation is mid-promotion; see
[`docs/rotation-promotion.md`](docs/rotation-promotion.md) for the active
shadow window and the cutover steps.

## Why

The scripts work. What they lack is a way to *notice*. Every incident this
operator is meant to prevent was detectable and undetected, because each left
the obvious signals green:

| incident | what was green | the number that would have caught it |
|---|---|---|
| fleet idle 8 hours | pods Running, panes healthy, providers with headroom, credentials valid | `hive_agent_idle_seconds` |
| spoke at 190% of budget, governor suppressing kicks | everything — suppression is *correct* behaviour | `hive_budget_exhausted` |
| 4 agents on a dead credential for 2 days | the spoke had no rotation coverage at all | `hive_credential_present` |
| 609 evicted pods → ingress down | node `Ready`, EC2 status checks passing | node metrics + `hive_spoke_reachable` |

None needed a cleverer controller. They needed an alertable series. That is the
point of this project; the controllers are how the numbers stay honest.

## Model

- **`HiveSpoke`** — one hive instance: namespace, org, installation, repos,
  budget, pins, holds. Status carries agents, providers, budget and idle times.
- **`ModelLadder`** — the placement ladder, `rank(benchmark) ∪ builtin`, gated by
  what the backends actually offer.
- **`SharedAuth`** — one credential store shared across spokes, verified by
  write-through.

### Reconcile modes

Every controller is `Observe` → `Shadow` → `Enforce`, per object.

`Shadow` computes the full action set and records it (status, events, metrics
with `applied="false"`) without applying any of it — diff that against the
incumbent CronJob before promoting. Only move to `Enforce` once the shadow
output matches, **and suspend the corresponding CronJob in the same change**.
Two control planes issuing the same mutations is worse than none.

## Things that are true and not obvious

Hard-won; a rewrite will reintroduce every one of these unless it knows them.

**Auth is two credentials, not one.** `X-Hive-Internal: <token>` authenticates
*reads*; every mutation returns `owner access required`, and forged
`X-Hive-User`/`X-Hive-Role` headers are rejected. Writes need
`Cookie: hive_session=<id>` — underscore, not hyphen; `hive-session-v1` appears
in the binary and is not the cookie name. Sessions come only from a GitHub
device-flow login and live in `/data/dashboard-sessions.json`.

**Don't judge session expiry locally.** The store writes the pod's UTC offset,
callers write their own, and a lexicographic ISO-8601 compare across differing
offsets silently discards live sessions near the boundary. Take the newest by
expiry and let the server decide.

**`/api/status` lags writes by more than a read cycle.** It reported the old
model long after both calls returned `ok`. The authority is
`hive.yaml.runtime` and `hive-state.json` (`model_override`/`backend_override`).

**Switching backend and model is two calls with no transaction.** Between them
the agent sits on the new backend with the old model — `codex/claude-sonnet-5`
cannot launch. Verify after, and expect to retry.

**Rotation's own pauses are byte-identical to an operator's.** Rotation
authenticates with the owner session cookie, so a strand records as
`reason: "manual pause"`, `trigger: "dashboard-api"`, `by: <session owner>`.
The `stranded` journal is the only discriminator — and its rows name the
provider *at strand time*, which placement then moves away from, so a stale row
can point at a provider the agent no longer uses.

**`agents_due: null` means budget suppression as often as it means a stall.**
Check `budget.BUDGET_EXHAUSTED` before concluding anything, and never nudge past
it: that spends money someone explicitly capped.

**The ConfigMap is read once, at first boot.** After that it is inert forever.
Runtime config lives on the PVC; a stale persisted file silently downgrades to
compiled-in bootstrap defaults.

**A benchmark feed does not cover every provider you run.** Measured: zero
DeepSeek entries, and the only Google entries were models the CLI does not
offer. Letting the feed *replace* the built-in ladder deletes whole providers.
Union them, de-duplicated on provider **and model** — scoring a provider says
nothing about whether the rungs you actually run were the ones scored.

**Bands are scale-specific.** Thresholds carried across benchmarks made T1 empty:
cut-offs from a suite topping out at 89.5 matched nothing on an index that tops
out at 58.2, and every T1 agent would have stranded.

**A rung the backend doesn't offer must never be placed.** An agent launched on
an id the CLI rejects dies at startup and reads as a dead agent, not a bad
config. Gate the ladder on the live model list.

**hostPath and subPath can silently resolve to the wrong filesystem.** On this
node the same hostPath string reached device `0:66` for an existing PV and
`0:59` — an empty `DirectoryOrCreate` — for a freshly created one. `ls` renders
both plausibly. Only a write-through test distinguishes them.

**`$HOME` inside pod exec is `/root`, not the agents' home.** Spell out
`/data/home`, or every check inspects the wrong directory and reports a healthy
fleet as broken.

**busybox `date -d` rejects the state file's timestamps** (fractional seconds
plus numeric offset), so age arithmetic belongs in the hive pod, which has GNU
date. A checker that silently `continue`s on every agent prints exactly what a
healthy fleet prints.

**Unmeasured ≠ exhausted.** An unmeasured provider must stay eligible; a
positive reading at 100 must evacuate. Conflating them either strands a healthy
fleet or keeps filling a dead pool. `-1` is the sentinel.

## Metrics

`hive_agent_idle_seconds`, `hive_agent_paused{trigger}`, `hive_agents{state}`,
`hive_provider_used_percent`, `hive_budget_{used,limit}_tokens`,
`hive_budget_exhausted`, `hive_shared_auth_consistent{namespace,dir}`,
`hive_credential_present`, `hive_spoke_reachable`, `hive_reconcile_mode`,
`hive_actions_total{applied}`, `hive_reconcile_errors_total`.

Two alerts worth having on day one:

```promql
# a fleet that stopped for no reason (idle beyond any cadence, budget NOT the cause)
hive_agent_idle_seconds > 28800 and on(spoke) hive_budget_exhausted == 0

# a credential store that is no longer shared — a login will not propagate
hive_shared_auth_consistent == 0
```

## Develop

```bash
make generate manifests   # deepcopy + CRDs + RBAC, all via `go run`
make build test
make run                  # against your current kubecontext; dashboard on :8082
```

```bash
kubectl apply -f config/crd
kubectl apply -f config/rbac
kubectl apply -f config/manager
kubectl apply -f config/samples/fleet.yaml
```

## Roadmap

1. ~~`SharedAuth` (shadow)~~ — done; promote to Enforce and suspend
   `hive-shared-auth`.
2. ~~`ModelLadder`~~ — done (PR #15): inventory gate, benchmark union, and
   band derivation are implemented. It stays read-only by design — rotation
   consumes `Status.Effective`, `ModelLadder` itself never applies anything.
3. ~~`Rotation` shadow controller~~ — done (PR #15): provider probes and
   placement now run in Shadow across all three spokes. Promoting to Enforce
   and suspending the legacy `hive-rotate*` CronJobs is tracked in
   [`docs/rotation-promotion.md`](docs/rotation-promotion.md).
4. `Watchdog` — pane classification and healing.
5. `Nudge` — the budget-aware kick backstop.
6. `Pace` — burn-rate pacing.
