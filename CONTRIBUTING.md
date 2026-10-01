# Contributing to hive-operator

Thanks for your interest in contributing to hive-operator! This guide covers setting up your development environment, building and testing, and submitting changes.

## Getting Started

### Prerequisites

hive-operator is a Kubernetes operator written in Go. You'll need:

- **Go**: Version 1.22 or later. Install from [golang.org](https://golang.org/doc/install).
- **Kubernetes cluster**: A local cluster for testing (kind, minikube, or similar).
  - `kubectl` configured to access your cluster
  - `kind` or `minikube` for local development
- **make**: For running build targets.
- **Docker**: To build container images (optional for local testing).

### Clone and Setup

```bash
git clone https://github.com/tuna-os/hive-operator.git
cd hive-operator
make generate manifests   # Generate deepcopy methods, CRDs, RBAC
make build test           # Compile and run tests
```

## Development Workflow

### Building

Generate all artifacts (deepcopy, CRDs, RBAC) and compile:

```bash
make generate manifests
make build
```

### Testing

Run the full test suite:

```bash
make test
```

Tests verify controller logic, reconciliation, and metrics.

### Running Locally

Run the operator against your current Kubernetes context:

```bash
make run
```

This starts the operator and the dashboard on port `8082`. You can then apply sample resources:

```bash
kubectl apply -f config/samples/fleet.yaml
```

### Deploying to a Cluster

Deploy the operator, CRDs, and RBAC to your cluster:

```bash
kubectl apply -f config/crd
kubectl apply -f config/rbac
kubectl apply -f config/manager
kubectl apply -f config/samples/fleet.yaml
```

Then use `kubectl logs` to follow the operator:

```bash
kubectl logs -f deployment/hive-operator-controller-manager -n hive-operator-system
```

## Code Standards

- **Go version**: Maintain compatibility with Go 1.22+.
- **Code generation**: All deepcopy and CRD files are auto-generated. Edit the source types and run `make generate manifests` — never edit generated files directly.
- **Testing**: Add tests for new controllers and reconcilers. Tests should cover both happy paths and error cases.
- **Metrics**: Add metrics to `pkg/metrics/` if your controller exposes observability signals. Document new metrics in the README.

## Controllers and Key Concepts

The operator implements three main controllers:

1. **HiveSpoke** — one hive instance (namespace, org, repos, budget)
2. **ModelLadder** — provider placement with benchmarking
3. **SharedAuth** — credential store verification

### Reconcile Modes

All controllers follow a progression: `Observe` → `Shadow` → `Enforce`

- **Observe**: Read-only inspection; no mutations.
- **Shadow**: Compute full action set and record it (status, events, metrics with `applied="false"`); don't apply.
- **Enforce**: Apply all mutations. Promote here only after shadow output matches the incumbent system.

## Submitting Changes

### Before You Push

1. Run `make test` to verify your changes work.
2. Ensure you've run `make generate manifests` if you edited types.
3. Include a clear commit message explaining the "why" behind your change.
4. Sign your commits with DCO: `git commit -s`.

### Creating a Pull Request

1. Push your branch: `git push -u origin guide/your-branch-name`
2. Open a PR on GitHub. Link any related issues.
3. The CI suite will run automatically. If any check fails, review the details and fix the issue.

### PR Guidelines

- **Scope**: Keep PRs focused. One feature or controller improvement per PR when possible.
- **Commits**: Use clear commit messages. If your PR fixes an issue, mention it: `Fixes #123`.
- **Tests**: Add tests for new controller logic. Verify tests pass.
- **Docs**: Update the README if your changes affect user-facing behavior or metrics.
- **Migration notes**: If you change CRD schema or reconciliation logic, document breaking changes.

## Important Implementation Details

The README includes "Things that are true and not obvious" — hard-won lessons about authentication, session management, backend switching, and provider scheduling. Read these carefully before modifying those areas:

- **Auth is two credentials**: Internal reads use `X-Hive-Internal`; writes use `Cookie: hive_session`.
- **Don't judge session expiry locally**: Let the server decide.
- **`/api/status` lags writes**: Use runtime files as the authority.
- **Backend and model switching is not transactional**: Verify after, and expect to retry.
- **Budget suppression vs. stalls**: Check `budget.BUDGET_EXHAUSTED` before concluding a stall.

See the README for the full list.

## Metrics and Observability

Key metrics to monitor (listed in README):
- `hive_agent_idle_seconds`: Agent idle time
- `hive_budget_{used,limit}_tokens`: Budget tracking
- `hive_budget_exhausted`: Budget cap hit
- `hive_shared_auth_consistent`: Credential store health
- `hive_spoke_reachable`: Hive instance reachability

Add corresponding alerts for fleet health.

## Getting Help

- **Issues**: Use GitHub issues to report bugs or discuss improvements.
- **PR comments**: Ask questions about changes directly on PRs.
- **README**: Review the README for context on controllers, metrics, and design decisions.

## Recognition

All contributors are credited in the commit history. Commits must be signed with DCO (`git commit -s`) per project policy.
