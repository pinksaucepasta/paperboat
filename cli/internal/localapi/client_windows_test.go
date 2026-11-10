//go:build windows

package localapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestDialLocalUsesAuthenticatedPipeDialer(t *testing.T) {
	original := dialTrustedLocalPipe
	t.Cleanup(func() { dialTrustedLocalPipe = original })
	want := `\\.\pipe\paperboat-local-test`
	called := false
	dialTrustedLocalPipe = func(_ context.Context, path string) (net.Conn, error) {
		called = true
		if path != want {
			t.Fatalf("path=%q", path)
		}
		return nil, net.ErrClosed
	}
	if _, err := dialLocal(context.Background(), want, time.Second); err == nil || !called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

// A daemon whose pipe is gone (an update restarts its services on Windows) is ErrTransportUnavailable for every request the
// client makes, through the real pipe dialer: file-transfer renewals and stream opens wait for it instead of failing.
func TestMissingPipeIsTransportUnavailableForEveryRequest(t *testing.T) {
	client, err := NewClient(fmt.Sprintf(`\\.\pipe\paperboat-no-such-pipe-%d`, time.Now().UnixNano()), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Snapshot(ctx); !errors.Is(err, ErrTransportUnavailable) {
		t.Fatalf("snapshot over a missing pipe: %v, want ErrTransportUnavailable", err)
	}
	if _, err := client.OpenFileTransferStream(ctx, "handle-1"); !errors.Is(err, ErrTransportUnavailable) {
		t.Fatalf("file transfer stream over a missing pipe: %v, want ErrTransportUnavailable", err)
	}
}
