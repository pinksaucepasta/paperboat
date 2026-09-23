//go:build darwin || linux

package tunnel

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func TestLocalPeerDaemonRestartRemainsReconnectable(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "pb-reconnect-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	client, err := localapi.NewClient(root+"/daemon.sock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	peer := LocalPeerTunnel{Client: client}
	info := resolver.ConnectInfo{ProjectID: "machine_1", MachineGeneration: 1, Terminal: &resolver.TerminalTarget{
		EnvironmentID: "environment_1", SessionID: "session_1",
		Auth: resolver.AuthTarget{Token: "credential", ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)},
	}}
	if _, err = peer.Dial(context.Background(), info); !FallbackEligible(err) {
		t.Fatalf("temporary daemon absence stopped reconnect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = peer.Dial(ctx, info); FallbackEligible(err) {
		t.Fatal("canceled connection remained retryable")
	}
}
