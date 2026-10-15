package admission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStagingHandsOffBeforeCallerAndCredentialExpire(t *testing.T) {
	caller, cancel := context.WithCancel(t.Context())
	defer cancel()
	expires := time.Now().Add(20 * time.Millisecond)
	staged, err := Stage(caller, expires)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := staged.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	cancel()
	time.Sleep(time.Until(expires) + 20*time.Millisecond)
	if err := lease.Check(); err != nil {
		t.Fatal("setup lifetime canceled accepted connection", err)
	}
	lease.Revoke()
	if !errors.Is(lease.Check(), ErrRevoked) {
		t.Fatal("handoff lost revocation")
	}
}
func TestStagingCancellationBeforeHandoffRefusesAdmission(t *testing.T) {
	caller, cancel := context.WithCancel(t.Context())
	staged, err := Stage(caller, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Abort()
	cancel()
	if _, err := staged.Accept(); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled setup was admitted", err)
	}
}
