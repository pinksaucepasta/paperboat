//go:build darwin || linux

package runtime

import (
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// Every round-30 machine reported "Update to <its own version> failed" (updater_unavailable) on each
// heartbeat while its updater was healthy: the runtime asked /run/paperboat-updated/control.sock, but an
// installation's updater listens on its own socket (/run/paperboat-updated-u1000/control.sock), which hostd
// passes to the worker as PAPERBOAT_UPDATED_SOCKET. The worker's updater client must use that socket.
func TestProductionUpdaterClientUsesTheInstallationsUpdaterSocket(t *testing.T) {
	socket := filepath.Join(shorttemp.Dir(t), "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	client, err := newProductionUpdaterClient(func(key string) string {
		if key == "PAPERBOAT_UPDATED_SOCKET" {
			return socket
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	// The test listener does not run as root, so a client that reached it is refused on peer identity;
	// a client dialling any other path finds nothing there.
	if _, err := client.Status(t.Context()); errors.Is(err, updated.ErrUnavailable) || !errors.Is(err, updated.ErrControlDenied) {
		t.Fatalf("status error %v: the client did not dial the installation's updater socket %s", err, socket)
	}
	if _, err := newProductionUpdaterClient(func(string) string { return "" }); err == nil {
		t.Fatal("a worker without PAPERBOAT_UPDATED_SOCKET got an updater client for a guessed socket")
	}
}
