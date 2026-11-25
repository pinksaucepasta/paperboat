//go:build linux || darwin

package machineguard

import (
	"context"
	"errors"
	"testing"
)

// A request that ended while queued behind another one used to close the connection, and with it every lease on it.
func TestRequestEndedWhileQueuedKeepsTheConnection(t *testing.T) {
	conn, _ := unixPair(t)
	client := &Client{conn: conn}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Status(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("status returned %v; want context.Canceled", err)
	}
	if client.conn == nil {
		t.Fatal("a request that was never sent closed the guard connection")
	}
}
