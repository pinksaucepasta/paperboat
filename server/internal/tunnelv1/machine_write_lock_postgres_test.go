package tunnelv1

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
)

// A tunnel change rewrites its host machine's route assignments, which the machine's runtime attach writes too
// (prod 2026-11-20: 18 serialization failures in 30 min between such writers). It takes the host's write lock before
// its transaction: while another writer of that machine holds it, a pause waits, and it goes through once released.
func TestTunnelChangeQueuesBehindAnotherWriterOfItsMachineOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account, host := f.addAccount(t, "mlock")
	tunnel := "tun_mlock_" + f.suffix
	created := f.createTunnel(t, account, host, tunnel, "mlock-"+f.suffix)

	unlock, err := endpoints.LockMachine(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		hash := sha256.Sum256([]byte("mlock" + f.suffix))
		_, err := f.repository.Transition(ctx, StateRecord{OperationID: "op_mlock_" + f.suffix, AuditEventID: "aud_mlock_" + f.suffix, TunnelID: tunnel, AccountID: account,
			DesiredState: DesiredPaused, ExpectedGeneration: created.Tunnel.Generation, IdempotencyKey: "mlock-" + f.suffix, RequestHash: hash, ActorID: account,
			AuditActorID: account, ActorType: "user", CorrelationID: "corr_mlock_" + f.suffix})
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("the tunnel change ran while another writer held its machine (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tunnel change after the machine was released: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the tunnel change did not go through after the machine was released")
	}
}
