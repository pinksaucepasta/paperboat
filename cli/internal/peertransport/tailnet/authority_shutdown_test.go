package tailnet

import (
	"context"
	"testing"
)

func TestStoppingRenewalLeavesCarrierAuthorityUntilOwnerCloses(t *testing.T) {
	a, c, key, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, key, c)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := networkAPIFunc{configuration: func(context.Context, string) (string, error) { return "", context.Canceled }}
	_ = a.Run(ctx, client, nil)
	if _, err := a.Descriptor("machine_test"); err != nil {
		t.Fatal("stopping renewal closed the packet carrier before its owner could send QUIC close", err)
	}
	_ = a.Close()
	if _, err := a.Descriptor("machine_test"); err == nil {
		t.Fatal("owner close left peer authority usable")
	}
}
