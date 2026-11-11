//go:build linux || darwin

package carriercontrol

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// pbvm4 feat2: pb update failed with `Post "http://carrier/v1/carrier/control":
// EOF` because the control server's 5 s write timeout cut off a legitimately
// longer operation (terminal move up to 15 s).
func TestCarrierControlAnswersOperationsLongerThanFiveSeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the former write timeout")
	}
	ssh, err := managedssh.NewHost(managedssh.HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	root := shorttemp.Dir(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(root, "control.sock")
	token := []byte(strings.Repeat("c", 32))
	sha := strings.Repeat("a", 64)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := &Server{Endpoint: endpoint, Token: token, SHA256: sha, SSH: ssh, Ready: make(chan error, 1), ReleaseNetwork: func(ctx context.Context) error {
		select {
		case <-time.After(6 * time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(endpoint, token, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseNetwork(ctx); err != nil {
		t.Fatal("a 6 s network release lost its answer:", err)
	}
	cancel()
	<-done
}
