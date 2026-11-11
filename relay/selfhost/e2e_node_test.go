package selfhost

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
)

// TestE2ESelfhostNode is the edge-and-relay node of server/internal/app TestE2ECore: the installation supervisor
// pbh runs (Run: the claim endpoint, then the claimed components), with the binary launcher instead of Docker,
// so the real paperboat-tunnel and paperboat-relay built from the tree run as its children. It initializes the
// installation from PAPERBOAT_E2E_SELFHOST_CONFIG, writes a claim code to claim.json until claimed, and runs
// until SIGTERM. Started again on a claimed installation it only runs (TestE2ECore restarts it like a deploy).
func TestE2ESelfhostNode(t *testing.T) {
	dir, binaries := os.Getenv("PAPERBOAT_E2E_SELFHOST_DIR"), os.Getenv("PAPERBOAT_E2E_SELFHOST_BINARIES")
	if dir == "" {
		t.Skip("child process of server/internal/app TestE2ECore")
	}
	var config Config
	if err := json.Unmarshal([]byte(os.Getenv("PAPERBOAT_E2E_SELFHOST_CONFIG")), &config); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(dir, config); err != nil {
		t.Fatalf("initialize the edge installation: %v", err)
	}
	if ready, err := RuntimeReady(dir); err != nil {
		t.Fatalf("check the edge installation: %v", err)
	} else if !ready {
		code, address, _, err := CreateCode(dir)
		if err != nil {
			t.Fatalf("create the claim code: %v", err)
		}
		raw, _ := json.Marshal(map[string]string{"code": code, "address": address})
		if err := os.WriteFile(filepath.Join(dir, "claim.json"), raw, 0o600); err != nil {
			t.Fatalf("write the claim code: %v", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, dir, binaries, os.Stderr, nil); err != nil && ctx.Err() == nil {
		t.Fatalf("run the edge installation: %v", err)
	}
}
