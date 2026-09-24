# controller-gen and friends are run via `go run` so no global installs are
# needed and `make manifests` is never broken for the next person.
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0
IMG ?= ghcr.io/tuna-os/hive-operator:dev

.PHONY: all build test fmt vet generate manifests docker-build docker-build-usage deploy undeploy run
all: generate manifests build

generate:
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths=./api/...

manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=hive-operator paths=./... output:crd:artifacts:config=config/crd output:rbac:artifacts:config=config/rbac

fmt:  ; go fmt ./...
vet:  ; go vet ./...
test: ; go test ./... -count=1
build: fmt vet ; go build -o bin/manager ./cmd

run: ; go run ./cmd --leader-elect=false --dashboard-bind-address=:8082

USAGE_IMG ?= ghcr.io/tuna-os/hive-operator/hive-usage:dev
docker-build: ; docker build -t $(IMG) .
docker-build-usage: ; docker build -f Dockerfile.usage -t $(USAGE_IMG) .

deploy: manifests
	kubectl apply -f config/crd
	kubectl apply -f config/rbac
	kubectl apply -f config/manager

undeploy:
	-kubectl delete -f config/manager
	-kubectl delete -f config/rbac
	-kubectl delete -f config/crd
