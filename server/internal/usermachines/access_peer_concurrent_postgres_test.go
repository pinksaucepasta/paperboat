package usermachines

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

func TestCreateAccessSessionWaitsForPeerIdentityWritePostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	if _, err := f.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.peer_endpoint_certificates
(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
VALUES(sha256(convert_to($2,'UTF8')),$1,$2,'machine',1,1,decode(repeat('00',200),'hex'),sha256(convert_to('key'||$2,'UTF8')),
now()-interval '1 minute',now()+interval '1 day','test',sha256(convert_to('signer'||$2,'UTF8')))`, f.account, f.machine); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, query := range []string{
			`DELETE FROM paperboat.peer_network_identities WHERE user_id=$1`,
			`DELETE FROM paperboat.peer_endpoint_certificates WHERE user_id=$1`,
		} {
			if _, err := f.store.SQL().ExecContext(context.Background(), query, f.account); err != nil {
				t.Errorf("clean up peer fixture: %v", err)
			}
		}
	})
	if _, err := f.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.peer_network_identities
(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
VALUES($1,$2,'machine',$2,1,1,1,sha256(convert_to('wg'||$2,'UTF8')),sha256(convert_to($2,'UTF8')),'100.72.220.7',now(),now())`, f.account, f.machine); err != nil {
		t.Fatal(err)
	}
	writer, err := f.store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	var generation int64
	if err := writer.QueryRow(ctx, `UPDATE peer_network_identities SET config_generation=config_generation+1
 WHERE user_id=$1 AND endpoint_id=$2 RETURNING config_generation`, f.account, f.machine).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	in := dbsqlc.CreateUserMachineAccessSessionParams{
		ID: "access_" + f.suffix, UserMachineID: f.machine, UserID: f.account, EnvironmentID: "env_" + f.suffix,
		CLIClientSessionID: "cli_" + f.suffix, HelperTerminalSessionID: "jti_" + f.suffix, OperationID: "op_" + f.suffix,
		Capabilities: []string{"exec"}, ExpiresAt: time.Now().Add(time.Minute),
	}
	before := f.store.TransactionStats()
	done := make(chan error, 1)
	go func() { done <- f.service.createAccessSession(ctx, in) }()
	for waiting := false; !waiting; {
		select {
		case err := <-done:
			t.Fatalf("access finished before the peer identity write committed: %v", err)
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := f.store.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := f.store.TransactionStats()
	if n := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; n != 0 {
		t.Fatalf("access retried %d transaction attempts (40001/40P01)", n)
	}
	var observed int64
	if err := f.store.Pool().QueryRow(ctx, `SELECT config_generation FROM peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, f.account, f.machine).Scan(&observed); err != nil || observed != generation+1 {
		t.Fatalf("peer generation = %d, want %d, error %v", observed, generation+1, err)
	}
}
