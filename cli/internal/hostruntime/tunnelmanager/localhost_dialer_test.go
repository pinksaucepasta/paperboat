package tunnelmanager

import (
	"context"
	"net"
	"testing"
	"time"
)

// An app listening on only one loopback family is reached through "localhost",
// whichever family the resolver lists first (or lists at all).
func TestLocalhostDialerReachesEitherLoopbackFamily(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run(address, func(t *testing.T) {
			listener, err := net.Listen("tcp", address)
			if err != nil {
				t.Skipf("no listener on %s: %v", address, err)
			}
			defer listener.Close()
			go func() {
				if c, err := listener.Accept(); err == nil {
					c.Close()
				}
			}()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			connection, err := (&LocalhostDialer{}).DialContext(ctx, "tcp", net.JoinHostPort("LocalHost", port))
			if err != nil {
				t.Fatalf("dial localhost:%s: %v", port, err)
			}
			connection.Close()
		})
	}
}

func TestLocalhostDialerLeavesOtherAddressesAlone(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	// An explicit IPv6 address must not fall back to IPv4.
	if connection, err := (&LocalhostDialer{}).DialContext(context.Background(), "tcp", net.JoinHostPort("::1", port)); err == nil {
		connection.Close()
		t.Fatal("explicit ::1 reached an IPv4-only listener")
	}
}
