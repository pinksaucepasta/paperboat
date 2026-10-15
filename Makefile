.PHONY: build test smoke gate

# Linux host binaries in ignored .build/. Cross-platform device builds: see AGENTS.md.
build:
	@mkdir -p .build
	cd cli && CGO_ENABLED=0 go build -trimpath -o ../.build/pb ./cmd/pb
	cd relay && CGO_ENABLED=0 go build -trimpath -o ../.build/paperboat-relay ./cmd/paperboat-relay
	cd tunnel && CGO_ENABLED=0 go build -trimpath -o ../.build/paperboat-tunnel ./cmd/paperboat-tunnel
	cd server && CGO_ENABLED=0 go build -trimpath -o ../.build/paperboat-server ./cmd/paperboat-server

test:
	cd shared && go vet ./... && go test ./...
	cd cli && go vet ./... && go test ./...
	cd relay && go test ./...
	cd tunnel && go test ./...
	cd server && go test ./...

smoke:
	tools/device-smoke/smoke.sh --expect-version "$(VERSION)"

# Regression gate: run before every push (AGENTS.md).
gate:
	tools/gate.sh
