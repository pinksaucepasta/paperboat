package containers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPbhServerProbe is the skew gate's pbh step (tools/device-smoke/skew_check.sh, server/internal/app e2e): this
// package is built from the PREVIOUS release's source and run against a server built from HEAD, with the state of a
// claimed installation. It does what pbh does against the server on every update check and install: read readiness
// and ask for an update. It does nothing without the state directory.
func TestPbhServerProbe(t *testing.T) {
	dir := os.Getenv("PAPERBOAT_E2E_PBH_STATE_DIR")
	if dir == "" {
		t.Skip("set PAPERBOAT_E2E_PBH_STATE_DIR (skew gate)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := newNodeClient("tunnel", filepath.Join(dir, "tunnel"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.http.CloseIdleConnections()
	if _, err := client.readiness(ctx); err != nil {
		t.Fatalf("this pbh cannot read the server's node readiness: %v", err)
	}
	update, err := OpenUpdateNode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := update.Check(ctx, "0.0.0-skew"); err != nil && !errors.Is(err, ErrUpdateRequired) {
		t.Fatalf("this pbh cannot read the server's update answer: %v", err)
	}
}
