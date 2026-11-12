package tunnelv1

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
)

// A deleted tunnel frees its name at once; a live one still holds it (found on a real account: a tunnel deleted hours
// earlier answered tunnel_name_conflict on re-create).
func TestDeletedTunnelFreesItsNameOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account, host := f.accounts[0], "mch_trk07_primary_"+f.suffix
	first := f.createTunnel(t, account, host, "tun_reuse_a_"+f.suffix, "reuse-name")
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnels SET desired_state='deleted', deleted_at=now() WHERE id=$1`, first.Tunnel.ID); err != nil {
		t.Fatal(err)
	}
	second := f.createTunnel(t, account, host, "tun_reuse_b_"+f.suffix, "reuse-name")
	if second.Tunnel.ID == first.Tunnel.ID {
		t.Fatalf("the second tunnel reused the first one's row: %s", second.Tunnel.ID)
	}
	tunnelID := "tun_reuse_c_" + f.suffix
	hash := sha256.Sum256([]byte("create:" + tunnelID))
	endpointID := testutil.EndpointID("repository-fixture:" + tunnelID)
	_, err := f.repository.Create(ctx, CreateRecord{
		OperationID: "op_" + tunnelID, TunnelID: tunnelID,
		StableEndpointID: endpointID, StableEndpoint: "https://" + testutil.EndpointLabel(endpointID) + ".tunnels.example.test",
		AccountID: account, Name: "reuse-name", AccessMode: AccessPublic,
		Origin:         OriginRequest{Scheme: "http", Address: "127.0.0.1:3000"},
		IdempotencyKey: "create:" + tunnelID, RequestHash: hash, ActorID: account, AuditActorID: host,
		ActorType: "host", HostID: host, RequestID: "req_" + tunnelID, CorrelationID: "corr_" + tunnelID,
		SourceMachineID: host, AuditEventID: "aud_" + tunnelID,
	})
	if !errors.Is(err, ErrNameConflict) {
		t.Fatalf("a live tunnel with the same name: err = %v, want ErrNameConflict", err)
	}
}
