GO_VERSION := 1.27.1
GO_ROOT := $(shell GOTOOLCHAIN=go$(GO_VERSION) go env GOROOT)
export PATH := $(GO_ROOT)/bin:$(PATH)
GO := GOTOOLCHAIN=local go
GOFMT := $(GO_ROOT)/bin/gofmt
BUILD_FLAGS := -trimpath -buildvcs=false
OWNED_GO_FILES := $(shell find . -path ./vendor -prune -o -name '*.go' -print)

.PHONY: build check clean dependencies fmt fmt-check generate metrics-check metrics-generate source-policy test tidy verify-toolchain vet

dependencies:
	@./tools/verify-dependencies.sh

source-policy:
	@./tools/verify-source-policy.sh

metrics-generate:
	$(GO) run ./tools/metric-schema -write docs/metrics.json

metrics-check:
	$(GO) run ./tools/metric-schema docs/metrics.json

maintenance-check:
	@./tools/verify-repository-state.sh $(if $(CI),ci,development)

release-check:
	@./tools/verify-repository-state.sh release

verify-toolchain:
	@test "$$(GOTOOLCHAIN=local go env GOVERSION)" = "go$(GO_VERSION)" || { echo "required Go $(GO_VERSION), found $$(GOTOOLCHAIN=local go env GOVERSION)" >&2; exit 1; }

build: verify-toolchain
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -o bin/paperboat-tunnel ./cmd/paperboat-tunnel

fmt:
	@if test -n "$(OWNED_GO_FILES)"; then $(GOFMT) -w $(OWNED_GO_FILES); fi

fmt-check:
	@if test -n "$(OWNED_GO_FILES)"; then test -z "$$($(GOFMT) -l $(OWNED_GO_FILES))" || { $(GOFMT) -l $(OWNED_GO_FILES); echo "Go files are not formatted" >&2; exit 1; }; fi

generate:
	@if test -n "$(OWNED_GO_FILES)"; then $(GO) generate ./...; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

tidy:
	$(GO) mod tidy

check: verify-toolchain dependencies source-policy metrics-check fmt-check vet test build

clean:
	rm -rf bin dist coverage.out
