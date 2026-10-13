# Contributing to hive-operator

Thanks for your interest in contributing to hive-operator. This guide walks you through setting up your development environment, running tests, and submitting changes.

## Getting Started

### Prerequisites

hive-operator is written in Go and deployed as a Kubernetes operator. You'll need:

- **Go 1.21 or later**: Install from [go.dev](https://go.dev/dl/)
- **make**: Standard build utility (check with `make --version`)
- **Docker**: For building and pushing operator images
- **Kubernetes cluster**: A running cluster for local development
  - [kind](https://kind.sigs.k8s.io/) — lightweight local cluster (recommended for development)
  - [minikube](https://minikube.sigs.k8s.io/) — alternative option
  - Or an existing cluster with `kubectl` configured

### Clone and Build

```bash
git clone https://github.com/tuna-os/hive-operator.git
cd hive-operator
make generate manifests   # Generate deepcopy, CRDs, and RBAC
make build                # Build the operator binary
make test                 # Run the test suite
```

## Development Workflow

### Running the Operator Locally

The operator can run against your current kubecontext for local development:

```bash
# Apply the CRDs and RBAC rules
kubectl apply -f config/crd
kubectl apply -f config/rbac

# Run the operator in the foreground (logs go to stdout)
make run
```

The operator will watch the cluster and reconcile resources. The dashboard runs on `:8082`.

### Code Generation

When you add or modify CRD types, regenerate the manifests:

```bash
make generate    # Regenerate deepcopy and other generated code
make manifests   # Update RBAC and CRD YAML files
```

Commit both the modified source files and the generated manifests:

```bash
git add config/crd config/rbac
git commit -s -m "Update operator CRDs and RBAC"
```

### Testing

Run the full test suite:

```bash
make test
```

For a specific package:

```bash
go test -v ./controllers/...
```

With race detection enabled:

```bash
go test -race ./...
```

### Linting and Formatting

hive-operator uses standard Go tooling:

```bash
gofmt -w .     # Format all Go files
go vet ./...   # Run the linter
```

Both checks are enforced in CI.

### Building the Operator Image

To build the operator Docker image locally:

```bash
docker build -t hive-operator:dev -f Dockerfile .
docker build -t hive-operator-usage:dev -f Dockerfile.usage .
```

Push to a registry (after updating the image reference in manifests):

```bash
docker tag hive-operator:dev <registry>/hive-operator:dev
docker push <registry>/hive-operator:dev
```

### Testing with Sample Resources

Apply the sample HiveSpoke, HiveRelease, and HiveHousekeeping resources to test controller reconciliation:

```bash
kubectl apply -f config/samples/fleet.yaml
kubectl apply -f config/samples/release.yaml       # HiveRelease in Shadow mode
kubectl apply -f config/samples/housekeeping.yaml  # HiveHousekeeping in Shadow mode
kubectl apply -f config/usage/usagepools.yaml      # UsagePool resources
```

Watch the operator logs to see reconciliation in action:

```bash
kubectl logs -f deployment/hive-operator-controller-manager -n hive-operator-system
```

### Shadow Diff Testing

To verify that a controller change produces the same output as the bash-based original:

```bash
# Get logs from the bash rotate job
kubectl -n hive logs job/<latest hive-rotate job> > bash.log

# Get the current spoke state
kubectl get hivespoke school -o json > spoke.json

# Run the shadow diff tool
go run ./cmd/hive-shadow-diff --bash bash.log --spoke spoke.json
```

Exit code 0 means the outputs are identical. See DESIGN.md § 7 for how to classify differences before reporting them as bugs.

## Submitting Changes

### Commit Messages

- Keep the first line concise and descriptive (under 70 characters)
- Reference the issue number if applicable (`fixes #123`)
- Explain *why* the change is needed, not just what changed
- Sign your commits with `git commit -s` to add the Developer Certificate of Origin

### Pull Request Process

1. Create a feature branch from `main` and push to your fork or to the main repo
2. Open a pull request against `main`
3. Ensure all CI checks pass (tests, linting, formatting, build, image build)
4. Wait for review — maintainers will provide feedback if needed
5. Do not merge your own PR; a maintainer will merge once approved

### Before Opening a PR

- Run `make test` and verify all tests pass
- Run `make generate manifests` to ensure manifests are up-to-date
- Run `gofmt -w .` and `go vet ./...` to fix formatting and linting
- Build the operator image with `docker build` to catch build errors early
- Test with `make run` against a test cluster to verify behavior

## Architecture

Before contributing to controllers or significant features, familiarize yourself with:

- **DESIGN.md** — High-level architecture and critical invariants ("Things that are true and not obvious")
- **docs/rotation-promotion.md** — Rotation controller promotion steps
- **docs/liveness-promotion.md** — Watchdog and liveliness controller promotion
- **docs/release.md** — HiveRelease controller design
- **docs/housekeeping.md** — HiveHousekeeping controller and remaining CronJobs

The README lists the status of each controller (Observe, Shadow, Enforce) and the operations they support.

## Questions?

Open an issue on GitHub or reach out to the maintainers. We're happy to help. For architecture questions, check DESIGN.md first — it documents many non-obvious patterns.
