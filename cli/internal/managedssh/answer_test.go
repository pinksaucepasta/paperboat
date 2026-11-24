package managedssh

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// Whatever ends a served stream before sshd's first byte, the client is told why.
func TestServeNeverEndsAStreamWithoutAWord(t *testing.T) {
	for _, test := range []struct {
		name   string
		server func(net.Listener, context.CancelFunc, *Host)
		reason string
	}{
		{"sshd closes", func(l net.Listener, _ context.CancelFunc, _ *Host) {
			if c, err := l.Accept(); err == nil {
				_ = c.Close()
			}
		}, RefusedServerClosed},
		{"session ends", func(l net.Listener, cancel context.CancelFunc, _ *Host) {
			if c, err := l.Accept(); err == nil {
				defer c.Close()
				cancel()
				time.Sleep(200 * time.Millisecond)
			}
		}, RefusedInterrupted},
		{"access revoked", func(l net.Listener, _ context.CancelFunc, h *Host) {
			if c, err := l.Accept(); err == nil {
				defer c.Close()
				_ = h.Disable(context.Background())
				time.Sleep(200 * time.Millisecond)
			}
		}, RefusedNoAccess},
		{"sshd gone", nil, RefusedServerUnreachable},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener := loopbackListener(t)
			defer listener.Close()
			host, err := NewHost(HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			probed := make(chan struct{})
			go func() {
				defer close(probed)
				if c, err := listener.Accept(); err == nil {
					_ = c.Close()
				}
			}()
			if _, err := host.ReconcileTarget(t.Context(), 1, uint16(listener.Addr().(*net.TCPAddr).Port)); err != nil {
				t.Fatal(err)
			}
			<-probed
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.server == nil {
				_ = listener.Close()
			} else {
				go test.server(listener, cancel, host)
			}
			client, server := net.Pipe()
			defer client.Close()
			go func() { defer server.Close(); _, _ = host.Serve(ctx, 1, server) }()
			var refused *RefusedError
			if err := ReadServerStart(bufio.NewReader(client)); !errors.As(err, &refused) || refused.Reason != test.reason {
				t.Fatalf("client saw %v, want reason %s", err, test.reason)
			}
		})
	}
}
