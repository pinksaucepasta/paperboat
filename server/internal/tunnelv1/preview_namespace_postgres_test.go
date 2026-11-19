package tunnelv1

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestIndependentPreviewInsertsShareTLSNamespaceWithoutSerializationFailure(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	user, host := f.addAccount(t, "preview-first")
	other, otherHost := f.addAccount(t, "preview-second")
	tunnel := "tun_preview_tls_" + f.suffix
	f.createTunnel(t, user, host, tunnel, "preview-tls")
	hostSuffix := strings.ReplaceAll(f.suffix, "_", "-") + ".example.test"
	conflictHost := "preview-conflict-" + hostSuffix
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_routes
SET protocol='tls',match_hostname=$2,origin_scheme='tcp',path_prefix=NULL,preserve_host=false,
host_override=NULL,public_tcp_listener_id='listener_tls_443',public_tcp_port=443 WHERE tunnel_id=$1`,
		tunnel, conflictHost); err != nil {
		t.Fatal(err)
	}
	const insert = `INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,
owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline)
VALUES($1,'https://'||$2,$3,$3,$4,'session_preview','foreground','http','127.0.0.1:3000','public',now()+interval '1 hour')`
	begin := func() pgx.Tx {
		t.Helper()
		tx, err := f.database.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `SET LOCAL search_path TO paperboat; SET LOCAL enable_seqscan TO off`); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	first, second := begin(), begin()
	if _, err := first.Exec(ctx, insert, "preview_first_"+f.suffix, "preview-first-"+hostSuffix, user, host); err != nil {
		t.Fatal(err)
	}
	// The competing writer has its snapshot before the first finishes, as it
	// would after checking the owner and the publication plan limit.
	if _, err := second.Exec(ctx, `SELECT pg_current_snapshot()`); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Exec(ctx, insert, "preview_second_"+f.suffix, "preview-second-"+hostSuffix, other, otherHost); err != nil {
		t.Fatalf("independent preview insert: %v", err)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatalf("independent preview commit: %v", err)
	}

	conflicting := begin()
	_, err := conflicting.Exec(ctx, insert, "preview_conflict_"+f.suffix, conflictHost, other, otherHost)
	var conflict *pgconn.PgError
	if !errors.As(err, &conflict) || conflict.Code != "23505" || conflict.ConstraintName != "shared_443_hostname_mode_conflict" {
		t.Fatalf("preview taking a passthrough hostname: %v", err)
	}
}
