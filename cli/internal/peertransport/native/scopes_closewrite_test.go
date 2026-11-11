package native

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/admission"
)

func scopeConnOver(t *testing.T, parent context.Context) (*scopeConn, *admission.Lease) {
	t.Helper()
	lease, err := admission.Admit(parent, time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	t.Cleanup(func() { local.Close(); remote.Close() })
	return &scopeConn{Conn: local, lease: lease, stop: func() bool { return true }}, lease
}

// The FIN towards a client whose connection ended (the lease lives under it) or whose lease was closed is moot: the
// stream is already closed. Only a revoked grant is an admission failure.
func TestScopeConnCloseWriteAfterTheClientLeftIsClosedAndRevokedStaysAnAdmissionError(t *testing.T) {
	t.Run("the client's connection ended", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		conn, _ := scopeConnOver(t, parent)
		cancel()
		if err := conn.CloseWrite(); !errors.Is(err, net.ErrClosed) || errors.Is(err, tailnet.ErrAdmission) {
			t.Fatalf("CloseWrite after the connection ended = %v, want net.ErrClosed", err)
		}
	})
	t.Run("the lease was closed", func(t *testing.T) {
		conn, lease := scopeConnOver(t, context.Background())
		lease.Close()
		if err := conn.CloseWrite(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("CloseWrite after the lease closed = %v, want net.ErrClosed", err)
		}
	})
	t.Run("access was revoked", func(t *testing.T) {
		conn, lease := scopeConnOver(t, context.Background())
		lease.Revoke()
		if err := conn.CloseWrite(); !errors.Is(err, tailnet.ErrAdmission) || errors.Is(err, net.ErrClosed) {
			t.Fatalf("CloseWrite after a revocation = %v, want the admission error", err)
		}
	})
}
