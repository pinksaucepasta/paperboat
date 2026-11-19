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
	expires := time.Now().Add(time.Minute)
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
	serverClockAhead(t, 10*time.Minute) // the credential expires with no real wait
	if _, err := Stage(t.Context(), expires); !errors.Is(err, ErrExpired) {
		t.Fatal("the credential did not expire on the server clock", err)
	}
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
