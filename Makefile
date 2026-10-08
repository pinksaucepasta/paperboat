.PHONY: build test

build:
	@mkdir -p .build
	cd cli && CGO_ENABLED=0 go build -trimpath -o ../.build/pb ./cmd/pb
	cd relay && CGO_ENABLED=0 go build -trimpath -o ../.build/paperboat-relay ./cmd/paperboat-relay
	cd tunnel && CGO_ENABLED=0 go build -trimpath -o ../.build/paperboat-tunnel ./cmd/paperboat-tunnel

test:
	cd cli && go test ./internal/contracttest -run 'Test(PreviewTunnelV1HostOwnership|CredentialContractVector|NoiseOwnershipBoundary)$$' -count=1
	cd cli && go test ./internal/peertransport/tailnet -count=1
	cd relay && go test ./peerrelay ./derpquic -count=1
	cd tunnel && go test ./internal/contracttest ./cmd/paperboat-tunnel -count=1
