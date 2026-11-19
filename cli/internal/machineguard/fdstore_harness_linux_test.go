//go:build linux

package machineguard

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestHoldLeasesForFDStoreHarness is not a unit test: tools/guard-fdstore-check.sh compiles it (go test -c) and runs
// it inside a systemd container next to an installed guard. It takes PB_FDSTORE_HARNESS_LEASES leases so the guard's
// descriptor store holds that many sockets (as it does in production while daemons are connected), writes
// PB_FDSTORE_HARNESS_READY, and holds them until the guard goes away or the harness kills it.
func TestHoldLeasesForFDStoreHarness(t *testing.T) {
	count, _ := strconv.Atoi(os.Getenv("PB_FDSTORE_HARNESS_LEASES"))
	ready, domain := os.Getenv("PB_FDSTORE_HARNESS_READY"), os.Getenv("PB_FDSTORE_HARNESS_DOMAIN")
	if count == 0 || ready == "" || domain == "" {
		t.Skip("run by tools/guard-fdstore-check.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var client *Client
	for err := error(nil); client == nil; time.Sleep(100 * time.Millisecond) {
		if client, err = Connect(ctx, DefaultSocket); err != nil && ctx.Err() != nil {
			t.Fatalf("machine guard never answered: %v", err)
		}
	}
	defer client.Close()
	if _, err := client.Status(ctx); err != nil { // learns that the guard takes account/machine claims
		t.Fatal(err)
	}
	for i := 1; i <= count; i++ {
		hostname := fmt.Sprintf("m%d.%s", i, domain)
		address := netip.MustParseAddr(fmt.Sprintf("127.100.9.%d", i))
		if _, _, err := client.AcquireMachine(ctx, "harness-account", fmt.Sprintf("harness-machine-%d", i), hostname, address, 4000+i); err != nil {
			t.Fatalf("lease %d: %v", i, err)
		}
	}
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done() // the harness kills this process when the path under test is done
}
