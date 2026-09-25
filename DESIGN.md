# Usage pools and rotation: replacing the bash ops scripts

**Status:** Phase 1 (Observe) and the Phase 2 skeleton (Rotation in Shadow) are implemented on branch `usage-ccusage`. Nothing here mutates a hive.

**Goal:** move the provider-usage measurement, rotation and pacing now done by `hive-rotate.sh`, `hive-pace.sh` and `hive-tiers.sh` into the operator. We measure consumption with [ccusage](https://github.com/ccusage/ccusage) instead of scraping each CLI's TUI, and extend ccusage upstream where it falls short (`docs/upstream-ccusage.md`).

---

## 1. Phase 0 findings (real data, 2026-09-24)

### Method

- Every read was on copies; no pod data was modified. Logs were streamed out of `hive/hive-6f77b74574-qwtj9` and `hive-contributors/claude-contributor-…` with `kubectl exec … tar cf -`.
- Tool: ccusage 20.0.24, the static `@ccusage/ccusage-linux-x64` binary (4.7 MB, no Node).

### Where the logs are

**Who writes them.** hive v5 runs every agent as its own UID with `HOME=/data/home/agents/<name>`. Inside that home, `.claude`, `.codex`, `.gemini`, `.copilot` and `.config` are symlinks back to `/data/home`.

**What is shared.** `/data/home/.claude`, `.gemini` and `.codex` are three RWX PVCs. Every spoke mounts them (same inode in `hive`, `hive-reef` and `hive-hanthor`), and the contributor pods mount the Claude store too. The rest of `/data/home` is per-spoke (`hive-data`).

**Permissions.** Files are `dev:node` (1001:1000). Files are mode 660 and directories 2770.

### Sources

| source (CLI) | provider | works? | data location | per-agent attribution | 1 run on real volume | gaps |
|---|---|---|---|---|---|---|
| `claude` | anthropic | **yes** | `.claude/projects/-data-agents-<a>/*.jsonl` (shared; 120 MB, 190 sessions) | yes, from `projectPath`. Contributor sessions show as `-home-dev--local-state-hive-agent-cwd` | 1.0 s, 48 MB RSS | fleet-wide only: same project key on every spoke, so no per-spoke split. `claude-opus-5-5` is unpriced in 20.0.24 (see calibration) |
| `codex` | openai | **yes** | `.codex-<a>/sessions/**` (per-spoke, per-agent `CODEX_HOME`). Shared `.codex` holds only auth + `logs_2.sqlite` | yes, by running once per `CODEX_HOME`. Rows carry no home/cwd | 6.6 s across 10 homes, 1906 sessions | no `projectPath`; `--since` accepts dates only |
| `antigravity` (agy) | google | **yes, slowly** | `.gemini/antigravity-cli/conversations/*.db` (shared; 500 DBs, 206 MB, all modified within 8 days) | `projectPath` is the constant `"Antigravity"`. The sidecar scans each DB for `/data/agents/<a>` (76/80 found) | **185 s CPU** uncontended (747 s under load), about 0.5 s per DB | slow; no agent; `model_placeholder_m322` unpriced |
| `pi` | deepseek | **yes** | `.pi/agent/sessions/--data-agents-<a>--/` (per-spoke; 263 MB) | yes, from `projectPath` | 3.3 s, 134 MB RSS | none. No sessions since Sep 17 (DeepSeek balance is −$1.23) |
| `goose` | deepseek | parses, **0 rows** | `.local/share/goose/sessions/sessions.db` (5 sessions, Aug) | none | 0.07 s | effectively unused |
| `gemini` (Gemini CLI) | google | n/a | `.gemini/tmp` is empty | — | 0.05 s | not used by the fleet (agy is) |
| `copilot` | github | **no** | `.copilot/session-store.db` (`assistant_usage_events`) | — | — | ccusage reads `session-state/*/events.jsonl` or OTEL. This CLI version writes neither. Quota is premium requests anyway |
| `opencode` | — | n/a | not installed | — | — | — |
| muse (Meta) | meta | **no adapter** | `.local/share/muse/sessions` (`.msp-view-v1` journals) | — | — | no ccusage adapter, and Meta has no usage API |

**DeepSeek via which CLI?** pi (`[pi] deepseek-v4-flash`); goose was configured for `custom_deepseek` but is idle. `usage.ProviderOf` maps by model first, so pi rows for other providers would still land in the right pool.

**`--by-agent` is not what it sounds like.** In `ccusage daily --by-agent` the "agent" is the CLI source (claude, codex, …), not a hive agent. Hive-agent attribution only comes from session `projectPath`, per-home runs, or the DB scan.

**Unified reports are slow.** `ccusage daily --json --by-agent` over all sources took 399 s, dominated by antigravity. The sidecar therefore runs one source at a time and refreshes antigravity every 15 min instead of every 5.

**What doesn't work with symlinks.** ccusage does not follow symlinked files (`DirEntry::file_type`). A "recent files only" symlink view reads as 0 sessions. It also wouldn't have helped: every antigravity DB was touched within 8 days.

**What does work read-only.** Read-only SQLite with existing `-wal`/`-shm` files: a read-only copy parsed fine (34 of 40 sessions; the other 6 DBs have no usage table).

### What ccusage doesn't give

- **No remaining quota.** That needs provider APIs, which is by design.
- **No sub-day windows.** `--since` takes dates only. `blocks` exists for Claude only and anchors on the hour of the first message: it reported 15:00–20:00 while Anthropic's own window was 12:40–17:40.
- **No window model for any non-Claude provider.**

### Does hive v5 measure quota itself?

Yes, partly. `GET /api/providers/headroom` (owner-only) plus probers in `src/pkg/rotation/rotation.go`:

| provider | how v5 measures |
|---|---|
| anthropic | `api/oauth/usage` |
| openai | `codex app-server` `account/rateLimits/read` |
| google | `agy --print /usage` |
| deepseek | `user/balance` |
| github | premium_request usage |

- **It only works with `governor.rotation.enabled`.** Otherwise it runs publish-only into `~/.config/hive/contributor-quota/*.reading.json`.
- **On our pods it fails.** All 5 readings there are `{"state":"unknown","cause":"probe_failed"}` right now.
- **It's the natural long-term source.** These are the same endpoints the bash probes hit. Fixing it upstream (hivecommons) is the preferred route to *remaining* quota. ccusage then supplies what v5 can't: per-agent attribution, burn rate and history.

### Calibration: can consumption plus a limit replace the scraped "% used"?

**Anthropic: yes, within a few percent, once every model is priced.**

`hive/hive-provider-usage` (the bash probe) against shared-store consumption in the exact provider windows. The windows were cut from the JSONL by timestamp and run through ccusage, with `claude-opus-5-5` priced via `pricingOverrides`:

| reading | window | ccusage consumption | implied limit |
|---|---|---|---|
| 17:12Z, slot0 **34%** | 12:40–17:40 | $24.57 | **$72.3** |
| 18:15Z, slot0 **5%** | 17:40–22:40 | $3.81 | **$76.2** (±10%: 5% is coarse) |
| 17:12Z, slot1 **26%** | since 09-23T23:00 | $38.78 | **$149.2** |
| 18:15Z, slot1 **28%** | since 09-23T23:00 | $44.07 | **$157.4** (integer % → ±4%) |

Burn rate agrees with the pacer too. ccusage gives $24.57 over 4.5 h against a $72 cap, which is **7.5 %/h**; hive-pace fitted **8.04 %/h** (slot0). For the weekly slot, $2.13/h against $149 is **1.4 %/h**, against a fit of 1.73 %/h.

**Unpriced models zero a window.** Without the pricing fix, the 17:40 window read **$0.00** against Anthropic's 5%: all its spend was the contributor's `claude-opus-5-5`, which the 20.0.24 snapshot can't price. Hence the `Priced` condition and `config/usage/ccusage-config.yaml`.

**Cost beats tokens as a unit.** Raw token totals don't calibrate: 97.7% of Claude tokens are cache reads, and the implied token cap moved 117 M → 142 M between the two points.

**Google: not yet.** Between the two points agy spent $0.68 while the probe moved 52% → 56%. That implies a $17 cap, but the window is unknown (reset stayed 19:17Z). Two things break it:
- `model_placeholder_m322` is unpriced.
- `agy-contributor` spends the same account from its own `agy-home` PVC, which no spoke sidecar sees.

It needs the sidecar in the contributor pod plus a week of readings before a limit can be learned.

**OpenAI and DeepSeek: nothing to calibrate today.**
- OpenAI has sat at 100% weekly since before the logs start. The last codex session was Sep 23.
- DeepSeek is a prepaid balance, so its reading is only 0% or 100%. Its pool is for attribution and burn rate only.

### The current (pre-branch) shadow planner disagrees with bash on most of the fleet

On the live cluster, `school`'s `.status.rotationPlan` wanted to move 6+ healthy agents ("current rung is not in the effective ladder"), while bash printed `fleet already on the best available rung`. Four causes:

1. **Ladder mismatch.** The ModelLadder lacked bash's `gemini-3.6-flash-low` and `muse` rungs.
2. **No stickiness, no pace-demotion handling.**
3. **Effort-only "moves"** that bash never makes.
4. **`model` instead of `govModel`.**

The port below fixes all four. Its golden tests reproduce the bash job logs byte for byte.

---

## 2. Architecture

```
 hive pod (each spoke)                              hive-system
┌──────────────────────────────┐                ┌─────────────────────────────┐
│ hive  (upstream v5)          │                │ hive-operator                │
│   writes transcripts to      │                │  UsagePool controller ──┐    │
│   /data/home/…               │  HTTP :9464    │   GET /v1/usage?since=… │    │
│ hive-usage (this repo)  ◄────┼────────────────┼─  dedupe stores by       │    │
│   ccusage <src> session      │                │   fingerprint, calibrate │    │
│   → ledger (deltas)          │                │   limits, burn, ETA      │    │
│   /v1/usage  /metrics        │                │  HiveSpoke controller ◄──┘    │
│   RO mounts, own emptyDir    │                │   rotation.Compute (Shadow)   │
└──────────────────────────────┘                └─────────────────────────────┘
```

### Why a sidecar, not operator-side exec

**1. The binary has to live somewhere.** The hive image is upstream and has no ccusage in it. Exec-based collection would mean copying a binary into the pod's writable `/data`, which is a mutation of hive state. We can't do that in Observe, and it would be undone by pod rebuilds.

**2. Cost stays with the pod.** Antigravity parsing is ~3 CPU-minutes per pass. In a sidecar that CPU lands in the pod's own cgroup with its own limit (500m), not in `kubectl exec` streams through the API server every 2 minutes.

**3. State.** Windows need a ledger that survives between collections (§3). The sidecar keeps it in memory plus an emptyDir snapshot. An exec collector would have to keep that state in the operator for every pod.

**4. Least privilege.** The sidecar mounts `/data` and the shared stores readOnly, runs as UID 65532 / GID 1000 (group read is all it needs), has no credentials, and makes no network calls (`--offline`). The exec path runs as root in the hive container.

**5. Upgrade-safe.** `hive-upgrade` patches container `hive` by name with a strategic merge and judges only that container's status. The sidecar is a second, named container with no readiness probe, so it can never stall an upgrade or hold the pod NotReady.

**Cost of the choice:** one image to build (`Dockerfile.usage`, built by the same workflow) and a Deployment patch per spoke. Adding the patch restarts the pod once, because the Deployments use `Recreate`.

### Shared stores

A pool is an **account**, not a spoke. Three spokes' sidecars read the same Claude PVC, so the operator must count each store once.

**The problem.** Mount identity (st_dev/st_ino) differs between pods for the same directory: we measured dev 65 against 66. It can't be used to detect a shared store.

**The fix.** Each source reports a **content fingerprint**: a hash of the first 64 relative file paths. The controller counts the first namespace per (source, fingerprint) and marks the others `counted: false` in status and metrics.

---

## 3. The ledger: provider windows from cumulative sessions

`ccusage <source> session --json` gives cumulative per-session, per-model totals, with the owning project. Between two collections, each session's growth is what that agent spent in the interval. `internal/usage/ledger.go` handles it like this:

- **Measured deltas.** Each collection stores per-(source, session, model) totals and appends the positive delta as an entry at `clamp(lastActivity, prevCollection, now)`.
- **No negative spend.** Sessions that vanish (log cleanup) are not refunds, and shrinking totals clamp at zero.
- **Back-fill.** On first sight, a session's total is spread uniformly over its `[firstActivity, lastActivity]` in ≤1 h slices. This is approximate: on the Phase 0 copy the back-filled 5h Anthropic window read $16.4 against the exact $22.6. The `primed` flag marks it, and exactness arrives after one full window of measured deltas. A snapshot in the emptyDir survives container restarts; a pod restart back-fills again.
- **Queries.** `Since(t)` answers any window start to within the collection interval (5 min; 15 min for antigravity). The operator passes the provider-anchored window starts on each request.
- **Prometheus.** Entries also drive `hive_usage_tokens_total` / `hive_usage_cost_usd_total`, so `increase(...[5h])` works in Prometheus without the operator.

Once upstream accepts timestamp-precise `--since` or an events export (upstream draft #3), the ledger reduces to a cache.

---

## 4. API

### `UsagePool` (cluster-scoped, new)

```yaml
spec:
  provider: anthropic            # the account
  sources: [claude]              # default by provider
  namespaces: []                 # default: every HiveSpoke's namespace
  unit: costUSD                  # costUSD | tokens | outputTokens
  windows:
    - {name: 5h, duration: 5h, readingSlot: slot0}
    - {name: weekly, duration: 168h, readingSlot: slot1, limit: ""}   # "" = learn
  reading: {namespace: hive, name: hive-provider-usage, maxAge: 30m}  # transitional / fallback
  ccleft: {url: http://ccleft.hive.svc:9464, scope: "", maxAge: 30m}  # preferred reading source
status:
  windows[]:  start, resetsAt, consumed, limit, limitSource (configured|learned|none),
              learned, usedPercent, readingPercent, remaining, burnPerHour, exhaustionETA
  agents[]:   agent, window, consumed, share, models
  sources[]:  namespace, source, fingerprint, ok, counted, primed, error, unpricedModels
              readingSource (ccleft|configmap|none), providerWindow{id,unit,used,limit,remaining,remainingPercent,resetsAt}
  account:    source, provider, account, state, cause, plan, stale, fetchedAt, retryAt, homes, error   # from ccleft
  conditions: Ready, Reading (Ccleft | Fresh | Fallback | Unavailable), Priced (unpriced models)
```

- **Reading source: [ccleft](https://github.com/tuna-os/ccleft) first, the ConfigMap as fallback.** With `spec.ccleft` set, each window takes the provider's own remaining/limit/reset from ccleft's `GET /readings`. The window is `windows[].ccleftWindow` by id, or else the binding window whose kind matches the duration (≤6h `five_hour`, ≤48h `daily`, ≤14d `weekly`, else `monthly`) within `spec.ccleft.scope` (for agy, `gemini` rather than its `3p` pool). The operator imports ccleft's `Reading`/`Window` types as a Go module, so the schema cannot drift.
  - A reading counts when it carries a verdict (`ok`/`limited`/`exhausted`) measured within `maxAge`. A stale last-good that ccleft serves during 429s counts too, as long as it is young enough.
  - Otherwise the window falls back to the ConfigMap, and `status.account.error` says why: ccleft down, a 429 with no last-good yet, `auth_required`, or too old.
  - ccleft answers from its cache, so reading it never causes an upstream quota call. ccleft dedupes homes that share an account, which is why one ccleft per account pool replaces the per-spoke bash probes.

- **The limit** is `configured` if set. Otherwise it is **learned**: when a fresh reading ≥5% exists, `learned = EWMA(consumed / (pct/100))`. An observed exhaustion is the same equation at 100%.
- **`usedPercent`**, in order of preference:
  1. the provider's reading when fresh,
  2. otherwise `consumed/limit`,
  3. otherwise `-1`.

  `-1` is **unmeasured** and must never be treated as exhausted. That is the README invariant, carried through status, metrics and the dashboard.
- **Burn rate** is the last hour's consumption. **ETA** is `remaining / burn`, reported only if it falls before the reset.

### `HiveSpoke` additions

- **`spec.rotationUsageSource`**: `Probe` (the default) or `UsagePool`. Shadow must plan from the same readings as bash.
- **`spec.agentTiers`**: defaults to bash `AGENT_TIERS`.
- **`spec.rotation`**: thresholds, high-volume cadence, the agy cap, canaries, auto-resume, metered failover and peak windows, all defaulting to the `HIVE_ROTATE_*` values.
- **`status.agents[].cadence`**.
- **`status.rotationPlan[].action`** / **`fromProvider`**.
- **`status.rotationPlanText`**: the plan exactly as `hive-rotate.sh plan` prints it.
- **`status.rotationInputs`**: what the plan was computed from.

### Metrics

**From the sidecar:**

| metric | what it is |
|---|---|
| `hive_usage_tokens_total{source,provider,agent,model,kind}` | counter |
| `hive_usage_cost_usd_total{…}` | counter |
| `hive_usage_window_{cost_usd,tokens}{…,window=1h\|5h\|24h\|7d}` | gauge |
| `hive_usage_collect_success`, `_duration_seconds`, `hive_usage_last_success_timestamp_seconds` | collection health |
| `hive_usage_unpriced_model` | 1 per model ccusage can't price |

**From the operator:**

| metric | what it is |
|---|---|
| `hive_provider_usage_ratio{pool,provider,window}` | −1 when there is no limit |
| `hive_pool_used_percent{…,source=reading\|ccusage}` | what rotation would read |
| `hive_provider_reading_percent` | the provider's own reading |
| `hive_provider_consumed`, `hive_provider_remaining`, `hive_provider_limit{source}` | window totals |
| `hive_provider_burn_per_hour` | last hour's rate |
| `hive_provider_exhaustion_eta_seconds` | −1 when it won't be reached before reset |
| `hive_agent_usage{pool,provider,agent,window,unit}` | per-agent consumption |
| `hive_usage_source_up{…,counted}` | sidecar source health |
| `hive_rotation_plan_decisions{spoke,action,applied}` | current plan by action |

**Probe shadow-check alert:**

```promql
abs(hive_provider_reading_percent - 100*hive_provider_usage_ratio) > 10 and hive_provider_reading_percent >= 0
```

---

## 5. How Rotation and ModelLadder consume it

**The planner** is `internal/rotation.Compute`: a pure port of `hive-rotate.sh` apply/plan, rule for rule. Every rule is cited by bash line in the code.

What it covers:
- un-strand journal
- auto-resume: undeclared operator pauses, holds, peak holds, recovery net
- pins
- stickiness, only on **positive** exhaustion or login-block
- pace-demotion counted as in-tier
- `choose_rung`, including the cost rank, peak bit, `+5×assigned` spreading, the metered-failover exception and the high-volume/agy caps
- strands
- the openai canary with cooldown
- effort derived from the agy model suffix

**QUIRKs are reproduced, not fixed** (`planner.go`, noted in comments). Chief among them: within a provider, the alphabetically-first model wins the tie. Fix them after cut-over, one reviewed change at a time.

**Inputs:**
- agents from `/api/status` (`govModel`, `cadence`) overlaid with `hive-state.json` overrides
- rungs from `ModelLadder.status.effective`, de-duplicated on tier + provider + model
- readings from the Probe ConfigMap, or UsagePools when `rotationUsageSource: UsagePool`; a pool gives its worst window's `usedPercent`

**Ladder parity.** `config/samples/fleet.yaml` now carries bash's TIERS table verbatim. On the primary hive, bash also unions `/state/hive-rotate/tiers.tsv` (the hive-tiers AA cache) in front of the table.

- **The effect:** T1 on school includes `claude-fable-5-1`, and pace-demotion reads as "from claude-fable-5-1".
- **The fix:** put those rows into `ModelLadder.spec.builtin` (or give the ladder the AA feed via `benchmarkURL`) before judging T1 diffs on school. Reef and hanthor never see the cache: their state dirs lack it.

**What the operator can't see yet.** Bash's journals live on the `hive-ops-state` PVC:
- `stranded`
- `pace-demoted`
- `peak-paused`
- `canary-cool-*`

The planner therefore:
- **infers** pace-demotion: the agent sits on `rung_down(X)` for some X in its tier,
- treats every dashboard-api pause as an operator pause,
- knows no cooldowns.

In Enforce the operator owns these journals, in status or a ConfigMap. Until then they are a *known* diff class (§7).

**Actuation.** `rotation.Actuator` / `HiveActuator` implements the bash call sequence: switch → model → effort, backend rollback on model failure, pause for strands, resume. It is tested against a fake API.

**It is not wired.** `cmd/main.go` passes no Actuator. A spoke set to `Enforce` plans as Shadow and says so in condition `RotationEnforced=False, reason NoActuator`. (The earlier Enforce path, which also kicked, has been removed.)

**ModelLadder.** Unchanged here. The next step is pace: the pacer's demotion ladder (`rung_down`) plus the pool's `burnPerHour`, `remaining` and `exhaustionETA` replace hive-pace's least-squares fit on scraped percentages. hive-pace's verdict is `pressure = observed_rate / allowed_rate`, where `allowed_rate = (100 − pct) / hours_left`. The pool gives `burn / (remaining / hours_to_reset)` directly, per agent.

---

## 6. Cut-over plan, per CronJob

**Rule (from the README):** only move a controller to Enforce once its shadow output matches, and **suspend the corresponding CronJob in the same change**, in the same commit and the same `kubectl apply`.

| CronJob (ns hive) | replaced by | shadow evidence required | enforce change (one commit) |
|---|---|---|---|
| `hive-rotate`, `-reef`, `-hanthor` (*/20; apply mode) | HiveSpoke Rotation (`rotation.Compute` + `HiveActuator`) | 7 days of `hive-shadow-diff` = 0 differences on every tick for each spoke. Every remaining diff explained by a listed diff class (§7), then those classes closed by moving the journals into the operator | wire `HiveActuator` in `cmd/main.go` behind `--enable-rotation-enforce`; set `rotationMode: Enforce` on **one** spoke (reef first: no pins, no cache); `kubectl patch cronjob hive-rotate-reef -p '{"spec":{"suspend":true}}'` in the same change. Then hanthor, then school |
| the probe half of `hive-rotate` (`probe_all`, publishes `hive-provider-usage`) | UsagePool (consumption + learned limit), later hive v5 `/api/providers/headroom` | per pool, `\|reading% − 100×ratio\| ≤ 5` for 7 days (alert above), `Priced=True`, contributor pods covered | flip `rotationUsageSource: UsagePool` per spoke. The probe keeps publishing (calibration) until v5 headroom works; then point `UsagePool.spec.reading` at v5 and delete the bash probe |
| `hive-watchdog`, `-reef`, `-hanthor` (*/5) | Watchdog controller (roadmap 4) | pane classification diff against the job log | same pattern. Rotation must be enforced first: the watchdog's `choose_rung_healthy` escape hatch rotates |
| `hive-pace` (5,25,45) | Pace on UsagePool burn/ETA + `rung_down` | pace verdicts (hot/cold/on-pace) from pools match `hive-pace status` for 7 days | enforce + suspend `hive-pace`; the pacer's journal moves into operator status, and Rotation reads it (closes the pace-demotion inference) |
| `hive-tiers` (daily) | ModelLadder `benchmarkURL` + bands | `ladder.status.effective` ⊇ `tiers.tsv` rows | suspend `hive-tiers`; point the ladder at the AA feed |
| `hive-inventory` (daily) | stays (ModelLadder reads its ConfigMap) | — | — |
| `hive-shared-auth` (*/30) | SharedAuth (already in Shadow) | existing | enforce + suspend, independent of this work |
| `hive-nudge` | Nudge (roadmap 5) | — | — |
| `hive-peak-pause`/`-resume` | Rotation peak holds, later | — | — |

**The Enforce step itself is out of scope for this branch.** It needs its own review.

---

## 7. Shadow-diff method

1. **Collect the pair.** In the same tick, get the latest bash job log and the spoke:
   ```bash
   export KUBECONFIG=~/.kube/config-aws-migration
   job=$(kubectl -n hive get jobs -o json | jq -r '[.items[] | select(.metadata.ownerReferences[0].name=="hive-rotate")] | sort_by(.metadata.creationTimestamp) | last.metadata.name')
   kubectl -n hive logs job/$job > bash.log
   kubectl get hivespoke school -o json > spoke.json
   go run ./cmd/hive-shadow-diff --bash bash.log --spoke spoke.json   # exit 0 = identical
   ```
   Use `hive-rotate-reef` with `reef`, and `hive-rotate-hanthor` with `hanthor`.
2. **Line up the timing.** `.status.observedAt` must be within the bash job's tick. The spoke reconciles every 2 min and the job runs every 20. `/api/status` lags writes, so compare the tick *after* a bash apply only if the bash side changed nothing.
3. **Check the inputs.** `.status.rotationInputs` must say `Probe readings`. Both sides then read the same `hive-provider-usage` publication; bash reuses it for up to 20 min on spokes.
4. **Classify each difference:**
   - **(a) ladder** (tiers cache or inventory)
   - **(b) journals** (stranded, pace-demoted inference, peak-paused, canary cooldown)
   - **(c) snapshot skew** (a different `/api/status` read)
   - **(d) logic.** Only (d) blocks promotion. Add a golden test from the offending log before fixing it.
5. **Automate it.** Run steps 1–2 as a CronJob (read-only) and publish `hive_shadow_diff_agents{spoke,result}`, so a week of evidence is a query, not a chore. *Not built yet.*

---

## 8. Known gaps and risks

- **Contributor pods spend the same accounts from their own homes.** `agy-contributor` has its own `agy-home`, `pi-codex-contributor` has its own `pi-codex-home`. Their Claude store is the shared PVC, so Claude is covered. Add the sidecar there too, or accept that the learned limit absorbs their spend: it does, but burn and ETA then under-read.
- **Per-spoke split is impossible for shared stores.** Claude and agy project keys are identical across spokes. Attribution is per agent name, fleet-wide.
- **Antigravity cost.** ~0.5 CPU-s per DB. At 500 DBs and a 500m limit, one pass takes ~6 min. A 15-min cadence is fine; the upstream fix is draft #2.
- **Back-filled windows are approximate** until one full window of measured deltas has accumulated after a (re)start. `primed` and back-fill are visible in status.
- **Read-only SQLite on a live WAL.** It worked on read-only copies with existing `-wal`/`-shm`. Verify on rollout via `hive_usage_collect_success{source="antigravity"}`.
- **Rotation's Enforce is intentionally unwired.** The earlier kick-after-place behaviour was dropped to match bash.
