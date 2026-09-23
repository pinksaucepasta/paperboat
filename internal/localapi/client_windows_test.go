//go:build windows

package localapi

import (
	"context"
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
