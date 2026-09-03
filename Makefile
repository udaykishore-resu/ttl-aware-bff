# TTL-aware BFF — developer entry point.
#
#   make            show this help
#   make build      compile all three binaries into ./bin
#   make compose-up bring up the full local stack
#
# Every target that a human is expected to run carries a `## comment`, which is
# what `make help` prints. A target without one is an internal step.

# --- Shell -----------------------------------------------------------------
# bash with pipefail: without it, `a | tee b` reports the exit status of tee and
# a failing build looks like a passing one.
#
# SHELL must be an executable path, not a command line — `/usr/bin/env bash` is
# a common idiom but GNU Make treats the whole string as one filename and fails
# with "No such file or directory". Resolve bash from PATH instead, falling
# back to the usual location.
BASH_PATH := $(shell command -v bash 2>/dev/null || echo /bin/bash)
SHELL := $(BASH_PATH)
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
# Recipes run in one shell invocation, so `cd` and variables carry across lines.
.ONESHELL:

# --- Project ---------------------------------------------------------------
MODULE      := github.com/udaykishore/ttl-aware-bff
BINARIES    := bff opsource exsource
BIN_DIR     := bin
DIST_DIR    := dist
COVER_FILE  := coverage.out
COVER_HTML  := coverage.html
CHART_DIR   := deploy/helm/ttl-aware-bff
TF_DIR      := deploy/terraform

# --- Version metadata injected into main.version/commit/buildDate ----------
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Identical to the flags in deploy/docker/Dockerfile.*, so a locally built
# binary reports the same version string as the containerised one.
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildDate=$(BUILD_DATE)

GO_BUILD_FLAGS := -trimpath -tags osusergo,netgo -ldflags "$(LDFLAGS)"

# --- Container images (docs/DESIGN-CONTRACT.md section 9) ------------------
REGISTRY   ?= ghcr.io/udaykishore
IMAGE_TAG  ?= $(VERSION)

# --- Coverage gate ---------------------------------------------------------
# Kept in step with COVERAGE_THRESHOLD in .github/workflows/ci.yaml.
COVERAGE_THRESHOLD ?= 75.0

# --- Load test -------------------------------------------------------------
K6_SCRIPT   ?= test/load/load.js
K6_BASE_URL ?= http://localhost:8080

# --- Colours (suppressed when not a terminal, e.g. in CI logs) -------------
ifneq (,$(findstring xterm,$(TERM)))
	BOLD  := $(shell tput bold 2>/dev/null || true)
	CYAN  := $(shell tput setaf 6 2>/dev/null || true)
	RESET := $(shell tput sgr0 2>/dev/null || true)
else
	BOLD  :=
	CYAN  :=
	RESET :=
endif

.PHONY: help
help: ## Show this help
	@echo ""
	@echo "$(BOLD)TTL-aware BFF$(RESET)  version $(VERSION)  commit $(COMMIT)"
	@echo ""
	@echo "$(BOLD)Targets:$(RESET)"
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  $(CYAN)%-16s$(RESET) %s\n", $$1, $$2}'
	@echo ""

# ===========================================================================
# Dependencies and code generation
# ===========================================================================
.PHONY: tidy
tidy: ## Tidy and verify go.mod / go.sum
	go mod tidy
	go mod verify
	@echo "go.mod and go.sum are tidy."

.PHONY: generate
generate: ## Regenerate the gRPC stubs from api/proto
	# Requires protoc, protoc-gen-go and protoc-gen-go-grpc on PATH.
	./scripts/gen-proto.sh

# ===========================================================================
# Build and run
# ===========================================================================
# --- Offline module overlay ------------------------------------------------
# See OFFLINE-BUILD.md. The committed go.mod is canonical and needs no flags;
# go.offline.mod redirects every vanity import path to its GitHub mirror for
# environments that can reach github.com but not proxy.golang.org.
OFFLINE_MODFILE := go.offline.mod
OFFLINE_ENV     := GOFLAGS="-mod=mod -modfile=$(OFFLINE_MODFILE)" GOPROXY=direct GOSUMDB=off GOPRIVATE='*' GOTOOLCHAIN=local

.PHONY: build-offline
build-offline: ## Compile without a module proxy (see OFFLINE-BUILD.md)
	@mkdir -p $(BIN_DIR)
	@for b in $(BINARIES); do \
		echo "  building $$b (offline overlay)"; \
		env $(OFFLINE_ENV) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$$b ./cmd/$$b; \
	done

.PHONY: test-offline
test-offline: ## Run unit tests without a module proxy
	env $(OFFLINE_ENV) go test -race ./...

.PHONY: deps-check
deps-check: ## Verify go.mod and go.offline.mod require the same modules
	@a=$$(mktemp); b=$$(mktemp); \
	awk '/^require \(/,/^\)/' go.mod | grep -oE '^\s+[^ ]+ v[^ ]+' | awk '{print $$1, $$2}' | sort > $$a; \
	awk '/^require \(/,/^\)/' $(OFFLINE_MODFILE) | grep -oE '^\s+[^ ]+ v[^ ]+' | awk '{print $$1, $$2}' | sort > $$b; \
	if diff -u $$a $$b; then \
		echo "go.mod and $(OFFLINE_MODFILE) agree"; \
	else \
		echo "go.mod and $(OFFLINE_MODFILE) have drifted; see OFFLINE-BUILD.md"; exit 1; \
	fi

.PHONY: build
build: ## Compile all three binaries into ./bin
	@mkdir -p $(BIN_DIR)
	@for binary in $(BINARIES); do
		echo "  building $$binary"
		CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o $(BIN_DIR)/$$binary ./cmd/$$binary
	done
	@echo "Built $(BINARIES) into $(BIN_DIR)/ (version $(VERSION))."

.PHONY: run
run: build ## Run the BFF locally against configs/bff.yaml
	# Assumes the data sources and Redis are reachable. `make compose-up` first,
	# or run the two stubs from ./bin by hand.
	./$(BIN_DIR)/bff --config configs/bff.yaml

# ===========================================================================
# Tests
# ===========================================================================
.PHONY: test
test: ## Run unit tests
	go test -count=1 -timeout=5m ./...

.PHONY: test-race
test-race: ## Run unit tests with the race detector
	# -shuffle=on catches tests that depend on execution order, which is how a
	# suite quietly stops testing what it claims to.
	go test -race -shuffle=on -count=1 -timeout=10m ./...

.PHONY: test-integration
test-integration: ## Run integration tests against the compose stack
	$(MAKE) compose-up
	@echo "Running integration tests..."
	@BFF_TEST_API_URL=http://127.0.0.1:8080 \
	 BFF_TEST_ADMIN_URL=http://127.0.0.1:9090 \
	 BFF_TEST_OPSOURCE_ADMIN_URL=http://127.0.0.1:9111 \
	 BFF_TEST_EXSOURCE_ADMIN_URL=http://127.0.0.1:9112 \
	 BFF_TEST_REDIS_ADDR=127.0.0.1:6379 \
	 go test -v -count=1 -timeout=20m -tags=integration ./test/integration/...

.PHONY: test-contract
test-contract: ## Run contract tests against the compose stack
	$(MAKE) compose-up
	@echo "Running contract tests..."
	@BFF_TEST_API_URL=http://127.0.0.1:8080 \
	 BFF_TEST_ADMIN_URL=http://127.0.0.1:9090 \
	 go test -v -count=1 -timeout=15m -tags=contract ./test/contract/...

.PHONY: cover
cover: ## Run tests with coverage and enforce the threshold
	go test -race -covermode=atomic \
		-coverprofile=$(COVER_FILE) \
		-coverpkg=./internal/...,./pkg/... \
		-timeout=10m ./...
	go tool cover -func=$(COVER_FILE) | tail -30
	go tool cover -html=$(COVER_FILE) -o $(COVER_HTML)
	@total=$$(go tool cover -func=$(COVER_FILE) | awk '/^total:/ {print $$3}' | tr -d '%')
	@# awk, not bash arithmetic: these are floats.
	@if awk -v t="$$total" -v m="$(COVERAGE_THRESHOLD)" 'BEGIN { exit (t >= m) ? 0 : 1 }'; then
		echo "Coverage $$total% meets the $(COVERAGE_THRESHOLD)% threshold. Report: $(COVER_HTML)"
	else
		echo "Coverage $$total% is BELOW the $(COVERAGE_THRESHOLD)% threshold."
		exit 1
	fi

# ===========================================================================
# Static analysis
# ===========================================================================
.PHONY: lint
lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || {
		echo "golangci-lint not found. Install it with:"
		echo "  go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.2"
		exit 1
	}
	golangci-lint run --config=.golangci.yaml --timeout=5m ./...

.PHONY: lint-fix
lint-fix: ## Run golangci-lint with --fix
	golangci-lint run --config=.golangci.yaml --timeout=5m --fix ./...

.PHONY: fmt
fmt: ## Format Go source (gofmt -s + goimports)
	gofmt -l -w -s $$(git ls-files '*.go' | grep -v '^third_party/')
	@command -v goimports >/dev/null 2>&1 && {
		goimports -w -local $(MODULE) $$(git ls-files '*.go' | grep -v '^third_party/')
	} || echo "goimports not installed; ran gofmt only."

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: vuln
vuln: ## Scan for vulnerabilities reachable from this code (govulncheck)
	@command -v govulncheck >/dev/null 2>&1 || {
		echo "Installing govulncheck..."
		go install golang.org/x/vuln/cmd/govulncheck@latest
	}
	govulncheck -show verbose ./...

# ===========================================================================
# Containers and the local stack
# ===========================================================================
.PHONY: docker-build
docker-build: ## Build all three container images
	@for binary in $(BINARIES); do
		case "$$binary" in
			bff)      image="$(REGISTRY)/ttl-aware-bff" ;;
			opsource) image="$(REGISTRY)/ttl-aware-bff-opsource" ;;
			exsource) image="$(REGISTRY)/ttl-aware-bff-exsource" ;;
		esac
		echo "  building $$image:$(IMAGE_TAG)"
		docker build \
			--file deploy/docker/Dockerfile.$$binary \
			--build-arg VERSION=$(VERSION) \
			--build-arg COMMIT=$(COMMIT) \
			--build-arg BUILD_DATE=$(BUILD_DATE) \
			--tag "$$image:$(IMAGE_TAG)" \
			--tag "$$image:latest" \
			.
	done
	@echo "Built all images at tag $(IMAGE_TAG)."

.PHONY: compose-up
compose-up: ## Start the local stack and wait for it to be healthy
	docker compose up --build --detach --wait --wait-timeout 300
	@echo ""
	@echo "  API         http://localhost:8080/api/v1"
	@echo "  Admin       http://localhost:9090/metrics"
	@echo "  Traces      http://localhost:16686"
	@echo "  Prometheus  http://localhost:9091"
	@echo "  Grafana     http://localhost:3000  (admin/admin)"
	@echo ""

.PHONY: compose-down
compose-down: ## Stop the local stack and remove its volumes
	docker compose down --volumes --remove-orphans

.PHONY: compose-logs
compose-logs: ## Tail the local stack's logs
	docker compose logs --follow --tail=100

# ===========================================================================
# Load testing
# ===========================================================================
.PHONY: k6-load
k6-load: ## Run the k6 load profile against the local stack
	@command -v k6 >/dev/null 2>&1 || {
		echo "k6 not found. See https://grafana.com/docs/k6/latest/set-up/install-k6/"
		exit 1
	}
	$(MAKE) compose-up
	# BASE_URL is read by the script so the same profile can be pointed at a
	# deployed environment: `make k6-load K6_BASE_URL=https://bff.dev.example.com`
	BASE_URL=$(K6_BASE_URL) k6 run $(K6_SCRIPT)

# ===========================================================================
# Deployment manifests
# ===========================================================================
.PHONY: helm-lint
helm-lint: ## Lint the Helm chart against every values file
	helm lint $(CHART_DIR) --values $(CHART_DIR)/values.yaml
	helm lint $(CHART_DIR) --values $(CHART_DIR)/values-dev.yaml
	helm lint $(CHART_DIR) --values $(CHART_DIR)/values-prod.yaml
	@echo "Chart lints clean against all three values files."

.PHONY: helm-template
helm-template: ## Render the chart and check the output parses as YAML
	@mkdir -p $(DIST_DIR)/rendered
	@for values in values.yaml values-dev.yaml values-prod.yaml; do
		echo "  rendering $$values"
		helm template ttl-aware-bff $(CHART_DIR) \
			--namespace bff \
			--values $(CHART_DIR)/$$values \
			> $(DIST_DIR)/rendered/$$values
		# Lint checks syntax; this proves the rendered output is real YAML.
		python3 -c "import sys,yaml; list(yaml.safe_load_all(open(sys.argv[1])))" \
			$(DIST_DIR)/rendered/$$values
	done
	@echo "Rendered manifests are in $(DIST_DIR)/rendered/."

.PHONY: kustomize-build
kustomize-build: ## Render the kustomize base and both overlays
	@mkdir -p $(DIST_DIR)/kustomize
	kubectl kustomize deploy/k8s                > $(DIST_DIR)/kustomize/base.yaml
	kubectl kustomize deploy/k8s/overlays/dev   > $(DIST_DIR)/kustomize/dev.yaml
	kubectl kustomize deploy/k8s/overlays/prod  > $(DIST_DIR)/kustomize/prod.yaml
	@echo "Rendered manifests are in $(DIST_DIR)/kustomize/."

.PHONY: tf-fmt
tf-fmt: ## Check Terraform formatting (use tf-fmt-write to fix)
	terraform -chdir=$(TF_DIR) fmt -recursive -check -diff

.PHONY: tf-fmt-write
tf-fmt-write: ## Rewrite Terraform files to canonical format
	terraform -chdir=$(TF_DIR) fmt -recursive

.PHONY: tf-validate
tf-validate: ## Validate the Terraform configuration
	terraform -chdir=$(TF_DIR) init -backend=false
	terraform -chdir=$(TF_DIR) validate

# ===========================================================================
# Housekeeping
# ===========================================================================
.PHONY: clean
clean: ## Remove build output, coverage reports and rendered manifests
	rm -rf $(BIN_DIR) $(DIST_DIR)
	rm -f $(COVER_FILE) $(COVER_HTML) coverage.txt
	go clean -testcache
	@echo 'Cleaned. Note: "make compose-down" is what removes the local stack.'

.PHONY: ci
ci: fmt vet lint test-race cover vuln ## Run everything CI runs, locally
	@echo "Local CI equivalent passed."
