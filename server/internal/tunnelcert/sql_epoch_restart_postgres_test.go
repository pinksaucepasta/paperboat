package tunnelcert

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// TestEngineEdgeRestartStagesCertificateForNewProcessEpochOnPostgres covers an
// engine edge whose process restarted without a generation bump. The old
// epoch's distribution row at the same assignment generation must not block
// staging the still-valid certificate for the new epoch.
func TestEngineEdgeRestartStagesCertificateForNewProcessEpochOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run TLS PostgreSQL acceptance")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(dsn), "_test") {
		t.Fatal("PAPERBOAT_TEST_DATABASE_DSN must name an isolated *_test database")
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	fixture := sqlCertificateFixture{suffix: fmt.Sprintf("%d", time.Now().UnixNano())}
	fixture.insert(t, database)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, fixture.accountID)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, fixture.edgeID)
	})
	store, err := NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)

	first := fixture.certificate(1, now)
	if err := store.PutStaged(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Queries().StageTunnelCertificateEdgeV1(ctx, stageParams(first, fixture, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Queries().MarkTunnelCertificateEdgeStateV1(ctx, readyParams(first, fixture, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Queries().MarkTunnelCertificateEdgeStateV1(ctx, activeParams(first, fixture, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(ctx, first.ID, 1, now); err != nil {
		t.Fatal(err)
	}

	// The edge process restarts: same node, same assignment generation 1,
	// new process epoch. The certificate is still valid and must be staged.
	restarted := fixture
	restarted.epoch = fixture.epoch + "_restarted"
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET process_epoch=$2 WHERE id=$1`, fixture.edgeID, restarted.epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Queries().StageTunnelCertificateEdgeV1(ctx, stageParams(first, restarted, now)); err != nil {
		t.Fatalf("stage certificate for restarted edge epoch: %v", err)
	}
	edges, err := database.Queries().ListTunnelCertificateEdgesV1(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged := 0
	for _, edge := range edges {
		if edge.EdgeProcessEpoch == restarted.epoch && edge.State == "staged" {
			staged++
		}
	}
	if staged != 1 {
		t.Fatalf("restarted epoch rows staged = %d, want 1 (rows=%d)", staged, len(edges))
	}
}
