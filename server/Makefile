# paperboat-server developer tasks.
#
# The server reads configuration from real environment variables (it has no
# dotenv loader), so these targets source .env.local before running Go. Each
# make recipe runs in its own shell, so the source + command must stay on one
# logical line.

CONFIG ?=
ENV_FILE ?= .env.local
GO_VERSION := 1.27.3
SQLC_VERSION := v1.30.0
GO_ROOT := $(shell GOTOOLCHAIN=go$(GO_VERSION) go env GOROOT)
export PATH := $(GO_ROOT)/bin:$(PATH)
GO := GOTOOLCHAIN=local go
GOFMT := $(GO_ROOT)/bin/gofmt
GO_FILES := $(shell find . -path ./.git -prune -o -name '*.go' -print)

# Load ENV_FILE (if present) into the environment, exporting every key.
load-env = set -a; [ -f $(ENV_FILE) ] && . ./$(ENV_FILE); set +a
config-arg = $(if $(strip $(CONFIG)),-config $(CONFIG),)

.PHONY: binary-size-check build check clean control-storm dependencies fmt fmt-check generate generate-check license-check metrics-check metrics-generate migrate race reproducible-builds run scenario-matrix-check scenario-matrix-generate source-policy static-analysis test tidy topology-fault-smoke topology-smoke verification verify-toolchain vet vulnerability-check standalone-build

dependencies:
	@./tools/verify-dependencies.sh

source-policy:
	@./tools/verify-source-policy.sh

metrics-generate:
	$(GO) run ./tools/metric-schema -write docs/metrics.json

metrics-check:
	$(GO) run ./tools/metric-schema docs/metrics.json

verify-toolchain:
	@test "$$(GOTOOLCHAIN=local go env GOVERSION)" = "go$(GO_VERSION)" || { echo "required Go $(GO_VERSION), found $$(GOTOOLCHAIN=local go env GOVERSION)" >&2; exit 1; }

build:
	$(GO) build ./...

standalone-build:
	GOWORK=off $(GO) build -mod=readonly ./...

## run: start the server with .env.local loaded (real providers)
run:
	$(load-env); $(GO) run ./cmd/paperboat-server serve $(config-arg)

## migrate: apply database migrations with .env.local loaded
migrate:
	$(load-env); $(GO) run ./cmd/paperboat-server migrate $(config-arg)

## generate: regenerate type-safe database access
generate:
	python3 tools/generate-sqlc-schema.py
	$(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

generate-check:
	@before="$$(git diff -- internal/db/dbsqlc)"; $(MAKE) generate >/dev/null; test "$$(git diff -- internal/db/dbsqlc)" = "$$before" || { echo "generated sqlc output is stale; run make generate" >&2; git diff -- internal/db/dbsqlc; exit 1; }

check: standalone-build verify-toolchain dependencies source-policy metrics-check scenario-matrix-check fmt-check generate-check vet test build

scenario-matrix-generate:
	$(GO) run ./tools/scenario-matrix testdata/topology/scenarios.json internal/testtopology/topology_test.go ../.github/server-generated/topology-matrix.json

scenario-matrix-check:
	$(GO) run ./tools/scenario-matrix -check testdata/topology/scenarios.json internal/testtopology/topology_test.go ../.github/server-generated/topology-matrix.json

## test: run the test suite
test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

reproducible-builds: verify-toolchain
	@./tools/verify-reproducible-builds.sh

binary-size-check: verify-toolchain
	@./tools/verify-binary-sizes.sh

static-analysis: verify-toolchain source-policy
	@./tools/verify-static-analysis.sh

vulnerability-check: verify-toolchain
	@./tools/verify-vulnerabilities.sh

license-check: verify-toolchain
	@./tools/verify-licenses.sh

control-storm:
	@./tools/run-control-storm.sh

topology-smoke:
	PAPERBOAT_TOPOLOGY_TEST=1 $(GO) test -tags=topology ./internal/testtopology -run '^TestConcurrentScopesOwnAndCleanOnlyTheirResources$$' -count=1 -v

topology-fault-smoke:
	PAPERBOAT_TOPOLOGY_TEST=1 $(GO) test -tags=topology ./internal/testtopology -run '^TestNetworkFaultInjectionAffectsOwnedUDPAndRecovers$$' -count=1 -v

verification: check race static-analysis vulnerability-check license-check control-storm

## vet: run go vet
vet:
	$(GO) vet ./...

## fmt: format the codebase
fmt:
	$(GOFMT) -w $(GO_FILES)

fmt-check:
	@test -z "$$($(GOFMT) -l $(GO_FILES))" || { $(GOFMT) -l $(GO_FILES); echo "Go files are not formatted" >&2; exit 1; }

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin dist coverage.out
