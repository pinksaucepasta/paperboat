package derpquic

import (
	"context"
	"errors"
	"testing"
)

func TestClosedCarrierReceiveReportsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  func() Carrier
	}{
		{"quic", func() Carrier { return NewClient(ClientConfig{}) }},
		{"wss", func() Carrier { return NewWSSClient(WSSClientConfig{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.new()
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			// Exercise both ready cancellation branches without requiring a network.
			for i := 0; i < 32; i++ {
				if _, _, err := c.RecvDetail(); !errors.Is(err, ErrClosed) {
					t.Fatalf("closed receive: %v", err)
				}
				if err := c.Connect(context.Background()); !errors.Is(err, ErrClosed) {
					t.Fatalf("closed connect: %v", err)
				}
			}
		})
	}
}
