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

	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
)

func TestCarrierEventPushesFenceStaleWorkers(t *testing.T) {
	ssh, err := managedssh.NewHost(managedssh.HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	root := shorttemp.Dir(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	controller, worker, epoch := activeControlWorker(t)
	lost := make(chan struct{})
	calls := make(chan string, 8)
	token, sha := []byte(strings.Repeat("c", 32)), strings.Repeat("a", 64)
	server := &Server{Endpoint: filepath.Join(root, "events.sock"), Token: token, SHA256: sha, SSH: ssh, Ready: make(chan error, 1),
		WorkerFence: func(_ context.Context, id string, epoch uint64) (func(), <-chan struct{}, error) {
			release, err := controller.AcquireActive(id, epoch)
			return release, lost, err
		},
		RefreshNetwork: func() { calls <- "refresh" }, EventsHealth: func(healthy bool) {
			if healthy {
				calls <- "recovered"
			} else {
				calls <- "unhealthy"
			}
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.Endpoint, token, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	active, err := client.BindWorker(worker, epoch)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := client.BindWorker(worker, epoch+1)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		call func(*Client) error
	}{
		{"refresh", func(c *Client) error { return c.RefreshNetwork(ctx) }},
		{"recovered", func(c *Client) error { return c.EventsHealth(ctx, true) }},
		{"unhealthy", func(c *Client) error { return c.EventsHealth(ctx, false) }},
	} {
		if err := test.call(client); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unbound %s: %v", test.name, err)
		}
		if err := test.call(stale); !errors.Is(err, ErrFenced) {
			t.Fatalf("stale %s: %v", test.name, err)
		}
		select {
		case got := <-calls:
			t.Fatalf("stale worker delivered %s", got)
		default:
		}
		if err := test.call(active); err != nil {
			t.Fatal(err)
		}
		if got := <-calls; got != test.name {
			t.Fatalf("got %s, want %s", got, test.name)
		}
	}
}
