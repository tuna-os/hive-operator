# Contributing to hive-operator

hive-operator is a Kubernetes operator for the tuna-os Hive fleet: the rotation, healing, credential-sharing and pacing that currently run as shell CronJobs, modelled as CRDs and controllers, with Prometheus metrics and fleet alerting.

**Status: early.** Two controllers exist; `HiveSpoke` is observe-only. `SharedAuth` defaults to Shadow. Nothing has been cut over yet.

See [tuna-os/.github/CODE_OF_CONDUCT.md](https://github.com/tuna-os/.github/blob/main/CODE_OF_CONDUCT.md) for community guidelines.

## Set Up

You need:
- Go 1.22 or later
- kubectl and a Kubernetes cluster (local or remote)
- make

```bash
git clone https://github.com/tuna-os/hive-operator
cd hive-operator
make generate manifests
```

## Build and Test

```bash
# Generate CRDs, RBAC, and deepcopy
make generate manifests

# Format and vet
make fmt vet

# Run tests
make test

# Build the manager binary
make build

# Run locally against your kubecontext (dashboard on :8082)
make run

# Deploy to cluster
make deploy
```

To test with sample resources:
```bash
kubectl apply -f config/samples/fleet.yaml
kubectl get hivefleets
```

## Architecture

hive-operator models Hive fleet operations as Kubernetes CRDs and controllers:

- **`HiveSpoke`** — one hive instance (namespace, org, repos, budget, pins, holds). Status carries agents, providers, budget and idle times. Currently observe-only.
- **`SharedAuth`** — credential sharing across the fleet. Currently defaults to Shadow mode; will support Enforce and suspend the shell-based `hive-shared-auth`.
- **`ModelLadder`** — the placement ladder (`rank(benchmark) ∪ builtin`), gated by band derivation. Planned.
- **`Rotation`** — provider probes and placement. Planned; significant complexity (~1400 lines of conditionals in existing `hive-rotate.sh`).
- **`Watchdog`** — pane classification and healing. Planned.
- **`Nudge`** — budget-aware kick backstop. Planned.
- **`Pace`** — burn-rate pacing. Planned.

Controllers reconcile these resources and emit metrics to Prometheus. See [README.md](README.md) for the full metric list.

## Code Conventions

- Follow Go style via `go fmt` and `go vet`
- Use `controller-gen` via `make generate` and `make manifests` (never install globally)
- Add doc comments to exported types and functions
- Controllers should be declarative (current state → desired state)
- Metrics must be instrumented: add `hive_*` metrics for observability
- All errors must be logged with context

## Pull Requests

1. Open or find an issue for your work
2. Branch from `main`: `git checkout -b feature/your-feature`
3. Write commit messages: `feat: …`, `fix: …`, `refactor: …`, `docs: …`
4. Before pushing:
   ```bash
   make generate manifests   # regenerate CRDs if you modified API
   make fmt vet test
   ```
5. Push and open a PR against `main`

For new controllers, plan before implementing: file an issue with the CRD structure and reconciliation logic.

## Early Status and Roadmap

This project is in early stages. The immediate roadmap is:
1. ~~`SharedAuth` (shadow)~~ — done; promote to Enforce
2. `ModelLadder` — inventory gate + placement
3. `Rotation` — provider probes and placement
4. `Watchdog` — pane healing
5. `Nudge` — budget backstop
6. `Pace` — burn-rate pacing

Nothing has been cut over to production yet. Feedback on the CRD design and metric choices is welcome.

## Alerting

Two alerts are worth setting up on day one:

```promql
# Fleet idle beyond cadence, not due to budget exhaustion
hive_agent_idle_seconds > 28800 and on(spoke) hive_budget_exhausted == 0

# Credential store no longer shared — login changes will not propagate
hive_shared_auth_consistent == 0
```

---

By contributing, you agree your contributions are licensed under the project license.
