# Contributing to hive-operator

Thank you for contributing to `hive-operator` — the Kubernetes operator for the TunaOS Hive fleet managing rotation, healing, credential sharing, release management, and pacing.

## Development Workflow

### Prerequisites

- [Go toolchain](https://go.dev/doc/install) (1.23+ recommended)
- `make` and `git`
- `kubectl` (configured for a local or development Kubernetes cluster)

### Local Build & Test

Before opening a pull request, run the code generation, manifests, build, and test suite:

```bash
make generate manifests   # Re-generates deepcopy routines, CRDs, and RBAC via `go run`
make build test           # Compiles operator binaries and runs unit tests
```

To run the operator locally against your active Kubernetes cluster:

```bash
make run                  # Runs operator locally; dashboard served on :8082
```

### Directory Structure

| Path | Description |
|---|---|
| `api/v1alpha1/` | Custom Resource Definitions (`HiveSpoke`, `ModelLadder`, `SharedAuth`, `HiveRelease`, `UsagePool`, `HiveHousekeeping`) |
| `cmd/` | Binary entrypoints (`main.go`, `hive-usage/`, `hive-shadow-diff/`) |
| `config/` | Kustomize manifests (`crd/`, `manager/`, `rbac/`, `samples/`, `usage/`) |
| `docs/` | Architecture and promotion operational runbooks |
| `internal/` | Controllers (`controller/`), dashboard (`dashboard/`), metrics (`metrics/`), usage (`usage/`), rotation (`rotation/`) |

## Commit Guidelines & DCO

All contributions must include a Developer Certificate of Origin (DCO) sign-off line:

```bash
git commit -s -m "docs: add CONTRIBUTING.md guide for development workflow"
```

<!-- hive-contribute-plea: donated-compute appeal, keep in sync across repos -->
## Contribute compute — no code needed

No time to write code? You can still push this project's backlog forward. A TunaOS AI-agent hive works on this repository. Lend the hive your AI subscription or API tokens, and your machine runs contributor tasks from this project's backlog.

- 🪸 [Contribute compute to the reef hive](https://reef.tunaos.org/contribute)
