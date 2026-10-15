package connectorprotocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelendpoint"
)

func TestEndpointIdentityMigrationPreservesBindingsAndFencesOldSessions(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if err = db.Migrate(ctx, database); err != nil {
			t.Fatal(err)
		}
	}
	// Seed the pre-cutover persisted format in this isolated migration database.
	// Current production writers and validators keep their single current format.
	if _, err = database.SQL().ExecContext(ctx, `ALTER TABLE paperboat.tunnels DROP CONSTRAINT tunnels_stable_endpoint_id_check`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAPERBOAT_TEST_SCHEMA_READY", "1")
	type before struct {
		fixture         *trk08PostgresFixture
		endpointID, url string
		payload         []byte
	}
	seeds := []before{}
	for index := 0; index < 2; index++ {
		f := newTRK08PostgresFixture(t)
		var endpointID, url string
		if err = f.database.SQL().QueryRowContext(ctx, `SELECT stable_endpoint_id,stable_endpoint FROM paperboat.tunnels WHERE id=$1`, f.tunnelID).Scan(&endpointID, &url); err != nil {
			t.Fatal(err)
		}
		endpointID = strings.TrimPrefix(endpointID, "endpoint_")
		if index == 1 {
			endpointID = endpointID[:14] + "1" + endpointID[15:]
		}
		var value map[string]any
		if err = json.Unmarshal(f.config.Payload, &value); err != nil {
			t.Fatal(err)
		}
		delete(value, "stable_endpoint_id")
		payload, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		hash := sha256.Sum256(payload)
		seedTx, err := f.database.SQL().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer seedTx.Rollback()
		if _, err = seedTx.ExecContext(ctx, `ALTER TABLE paperboat.tunnel_config_generations DISABLE TRIGGER tunnel_config_generations_immutable`); err != nil {
			t.Fatal(err)
		}
		if _, err = seedTx.ExecContext(ctx, `UPDATE paperboat.tunnels SET stable_endpoint_id=$2 WHERE id=$1`, f.tunnelID, endpointID); err != nil {
			t.Fatal(err)
		}
		if _, err = seedTx.ExecContext(ctx, `UPDATE paperboat.tunnel_config_generations SET snapshot=$2,content_hash=$3 WHERE tunnel_id=$1 AND generation=1`, f.tunnelID, payload, hash[:]); err != nil {
			t.Fatal(err)
		}
		if _, err = seedTx.ExecContext(ctx, `ALTER TABLE paperboat.tunnel_config_generations ENABLE TRIGGER tunnel_config_generations_immutable`); err != nil {
			t.Fatal(err)
		}
		if err = seedTx.Commit(); err != nil {
			t.Fatal(err)
		}

		seeds = append(seeds, before{f, endpointID, url, payload})
	}
	// Recreate the exact pre-cutover schema constraint before applying migration44.
	if _, err = database.SQL().ExecContext(ctx, `ALTER TABLE paperboat.tunnels ADD CONSTRAINT tunnels_stable_endpoint_id_check CHECK (stable_endpoint_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'::text)`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile(filepath.Join("..", "db", "migrations", "044_typed_endpoint_identity.sql"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var enabled string
	if err = database.SQL().QueryRowContext(ctx, `SELECT tgenabled FROM pg_trigger WHERE tgrelid='paperboat.tunnel_config_generations'::regclass AND tgname='tunnel_config_generations_immutable'`).Scan(&enabled); err != nil || enabled != "O" {
		t.Fatalf("immutable trigger not restored: %s %v", enabled, err)
	}
	for index, seed := range seeds {
		f := seed.fixture
		var endpointID, url string
		var generation int64
		if err = f.database.SQL().QueryRowContext(ctx, `SELECT stable_endpoint_id,stable_endpoint,generation FROM paperboat.tunnels WHERE id=$1`, f.tunnelID).Scan(&endpointID, &url, &generation); err != nil {
			t.Fatal(err)
		}
		if tunnelendpoint.ValidateID(endpointID) != nil || url != seed.url || generation != 2 {
			t.Fatalf("endpoint cutover id=%s url=%s generation=%d", endpointID, url, generation)
		}
		parsed, parseErr := uuid.Parse(strings.TrimPrefix(endpointID, "endpoint_"))
		if parseErr != nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 {
			t.Fatal("endpoint identity is not canonical UUIDv4")
		}
		if index == 0 && endpointID != "endpoint_"+seed.endpointID {
			t.Fatal("existing v4 identity suffix changed")
		}
		for version := int64(1); version <= 2; version++ {
			var payload, storedHash []byte
			var previous sql.NullInt64
			if err = f.database.SQL().QueryRowContext(ctx, `SELECT snapshot,content_hash,previous_generation FROM paperboat.tunnel_config_generations WHERE tunnel_id=$1 AND generation=$2`, f.tunnelID, version).Scan(&payload, &storedHash, &previous); err != nil {
				t.Fatal(err)
			}
			snapshot, snapshotErr := wire.NewSnapshot(f.tunnelID, uint64(version), payload)
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			digest := sha256.Sum256(snapshot.Payload)
			if !bytes.Equal(digest[:], storedHash) || !bytes.Equal(snapshot.Payload, payload) {
				t.Fatal("snapshot bytes/hash are not canonical")
			}
			var value map[string]any
			if err = json.Unmarshal(payload, &value); err != nil {
				t.Fatal(err)
			}
			if value["stable_endpoint_id"] != endpointID || value["stable_endpoint"] != seed.url {
				t.Fatal("snapshot lost exact endpoint binding")
			}
			delete(value, "stable_endpoint_id")
			value["generation"] = float64(1)
			original, marshalErr := json.Marshal(value)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if !bytes.Equal(original, seed.payload) {
				t.Fatal("configuration history changed beyond endpoint identity/generation")
			}
			if version == 2 && (!previous.Valid || previous.Int64 != 1) {
				t.Fatal("new configuration generation lost its predecessor")
			}
		}
		var current []byte
		if err = f.database.SQL().QueryRowContext(ctx, `SELECT snapshot FROM paperboat.tunnel_config_generations WHERE tunnel_id=$1 AND generation=2`, f.tunnelID).Scan(&current); err != nil {
			t.Fatal(err)
		}
		var changed map[string]any
		if err = json.Unmarshal(current, &changed); err != nil {
			t.Fatal(err)
		}
		changed["name"] = "changed configuration"
		mutated, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		mutatedHash := sha256.Sum256(mutated)
		_, mutationErr := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_config_generations SET snapshot=$2,content_hash=$3 WHERE tunnel_id=$1 AND generation=2`, f.tunnelID, mutated, mutatedHash[:])
		var rejected *pgconn.PgError
		if !errors.As(mutationErr, &rejected) || rejected.Code != "55000" {
			t.Fatalf("ordinary immutable configuration mutation accepted: %v", mutationErr)
		}
		connector := f.connectors[0]
		ref := wire.SessionRef{TunnelID: f.tunnelID, ConnectorID: connector.id, SessionID: connector.oldSessionID, ProcessGeneration: 1}
		oldHash := sha256.Sum256(seed.payload)
		ack := wire.Ack{AccountID: f.accountID, TunnelID: f.tunnelID, ConnectorID: connector.id, SessionID: connector.oldSessionID, ProcessGeneration: 1, Generation: 1, ContentHash: "sha256:" + hex.EncodeToString(oldHash[:]), Kind: wire.AckSnapshot, Status: wire.AckApplied}
		if err = f.store.RecordApplied(ctx, ref, ack); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("old session retained configuration admission: %v", err)
		}
		var state string
		var disconnected sql.NullTime
		if err = f.database.SQL().QueryRowContext(ctx, `SELECT state,disconnected_at FROM paperboat.tunnel_connector_sessions WHERE id=$1`, connector.oldSessionID).Scan(&state, &disconnected); err != nil || state != "disconnected" || !disconnected.Valid {
			t.Fatalf("old session fence %s %v", state, err)
		}
	}
}
