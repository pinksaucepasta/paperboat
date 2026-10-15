package admission

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestAdmittedConnectionOutlivesCredentialAndRevocationInterruptsIO(t *testing.T) {
	revoked := make(chan struct{})
	expires := time.Now().Add(20 * time.Millisecond)
	lease, err := Admit(t.Context(), expires, revoked)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	stop := lease.Bind(server)
	defer stop()
	time.Sleep(time.Until(expires) + 20*time.Millisecond)
	sent := make(chan error, 1)
	go func() { _, e := server.Write([]byte{42}); sent <- e }()
	client.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := io.ReadFull(client, b[:]); err != nil || b[0] != 42 {
		t.Fatalf("credential expiry closed admitted work: %v", err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if _, err := Admit(t.Context(), expires, nil); !errors.Is(err, ErrExpired) {
		t.Fatal("expired credential admitted new work", err)
	}
	blocked := make(chan error, 1)
	go func() { _, e := server.Read(b[:]); blocked <- e }()
	close(revoked)
	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("revocation did not interrupt I/O")
		}
	case <-time.After(time.Second):
		t.Fatal("revocation left blocked I/O")
	}
	if !errors.Is(lease.Check(), ErrRevoked) {
		t.Fatal("revocation cause lost", lease.Check())
	}
}

func TestWatchDoesNotMissWithdrawalDuringSubscription(t *testing.T) {
	changed := make(chan struct{})
	allowed := true
	lease, err := Admit(context.Background(), time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	err = lease.Watch(func() <-chan struct{} { allowed = false; close(changed); return changed }, func() bool { return allowed })
	if !errors.Is(err, ErrRevoked) {
		t.Fatal("withdrawal not observed", err)
	}
	revoked := lease.Context().Done()
	select {
	case <-revoked:
	case <-time.After(time.Second):
		t.Fatal("missed revocation during subscription")
	}
}

func TestPollBoundsLookupsAndStopJoinsWithoutRevoking(t *testing.T) {
	lease, err := Admit(t.Context(), time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	started, finished := make(chan struct{}), make(chan struct{})
	stop := lease.Poll(func(ctx context.Context) error {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > RevocationInterval {
			t.Error("authority lookup is unbounded")
		}
		close(started)
		defer close(finished)
		<-ctx.Done()
		return ctx.Err()
	})
	<-started
	stop()
	select {
	case <-finished:
	default:
		t.Fatal("stop returned before lookup joined")
	}
	if err := lease.Check(); err != nil {
		t.Fatal("stopping watcher revoked admitted work", err)
	}
}

func TestPollRetainsTransientFailureAndClosesOnAuthoritativeDenial(t *testing.T) {
	lease, err := Admit(t.Context(), time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	first := make(chan struct{})
	calls := 0
	stop := lease.Poll(func(context.Context) error {
		calls++
		if calls == 1 {
			close(first)
			return errors.New("authority temporarily unavailable")
		}
		return ErrRevoked
	})
	defer stop()
	<-first
	if err := lease.Check(); err != nil {
		t.Fatal("transient lookup failure revoked work", err)
	}
	select {
	case <-lease.Context().Done():
	case <-time.After(2 * RevocationInterval):
		t.Fatal("authoritative denial did not revoke")
	}
	if !errors.Is(lease.Check(), ErrRevoked) {
		t.Fatal("authoritative denial lost revocation cause")
	}
}
