//go:build linux || darwin

package carriercontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// An update hands the machine's network from the serving carrier to the
// candidate over this socket: the serving one releases it, then the candidate
// starts it. A carrier without the hook answers unavailable.
func TestNetworkHandoffOperationsRoundTrip(t *testing.T) {
	ssh, err := managedssh.NewHost(managedssh.HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	root := shorttemp.Dir(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	token, sha := []byte(strings.Repeat("c", 32)), strings.Repeat("a", 64)
	var calls []string
	server := &Server{Endpoint: filepath.Join(root, "control.sock"), Token: token, SHA256: sha, SSH: ssh, Ready: make(chan error, 1),
		ReleaseNetwork: func(context.Context) error { calls = append(calls, "release"); return nil },
		StartNetwork:   func(context.Context) error { calls = append(calls, "start"); return nil }}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.Endpoint, token, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseNetwork(ctx); err != nil {
		t.Fatal("release-network did not round trip", err)
	}
	if err := client.StartNetwork(ctx); err != nil {
		t.Fatal("start-network did not round trip", err)
	}
	if strings.Join(calls, ",") != "release,start" {
		t.Fatal("hooks ran as", calls)
	}
	server.StartNetwork = nil
	if err := client.StartNetwork(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("a carrier without the hook started a network", err)
	}
	cancel()
	<-done
}
