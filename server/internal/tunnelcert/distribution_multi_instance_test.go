package tunnelcert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// F21-52: a certificate action staged on one server instance is pulled and acknowledged by the edge on another,
// and the staging instance sees the result. Before the shared store each instance kept its own pending map.
func TestDistributionActionStagedOnOneInstanceIsPulledAndAckedOnAnother(t *testing.T) {
	stagedOnOneInstanceAckedOnAnother(t, NewMemoryDistributionActionStore(), "epoch_multi_001")
}

func TestDistributionActionStagedOnOneInstanceIsPulledAndAckedOnAnotherOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the shared distribution store on PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLDistributionActionStore(database)
	if err != nil {
		t.Fatal(err)
	}
	epoch := fmt.Sprintf("epoch_pg_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = database.Pool().Exec(context.Background(), `DELETE FROM paperboat.tunnel_certificate_distribution_actions WHERE edge_process_epoch=$1`, epoch)
	})
	stagedOnOneInstanceAckedOnAnother(t, store, epoch)
	var plaintext bool
	if err := database.Pool().QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM paperboat.tunnel_certificate_distribution_actions WHERE edge_process_epoch=$1 AND (position('private-key-secret' IN message::text) > 0 OR sealed_private_key IS NOT NULL))`, epoch).Scan(&plaintext); err != nil || plaintext {
		t.Fatalf("an acknowledged action kept key material: %v %v", plaintext, err)
	}
}

func stagedOnOneInstanceAckedOnAnother(t *testing.T, store DistributionActionStore, epoch string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	identity := DistributionNodeIdentity{NodeID: "edge_multi", ProcessEpoch: epoch}
	hub := func() *DistributionHub {
		h, err := NewDistributionHub(DistributionHubConfig{Store: store, Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
			Credential: []byte("distribution-credential-multi-012345678901234567890123456789"),
			Now:        func() time.Time { return now },
			Identity: DistributionNodeIdentityResolverFunc(func(context.Context, *http.Request) (DistributionNodeIdentity, error) {
				return identity, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	first, second := hub(), hub()
	request := trk35DistributionRequest(1, now, identity)
	if _, err := first.enqueue(context.Background(), "stage", request); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(distributionPull{EdgeNodeID: identity.NodeID, EdgeProcessEpoch: identity.ProcessEpoch})
	pull := httptest.NewRequest(http.MethodPost, CertificateDistributionPullPath, strings.NewReader(string(body)))
	pull.Header.Set("Content-Type", "application/json")
	pull = pull.WithContext(withDistributionNodeIdentity(context.Background(), identity))
	recorder := httptest.NewRecorder()
	second.handlePull(recorder, pull)
	var page distributionPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil || recorder.Code != http.StatusOK || len(page.Messages) != 1 {
		t.Fatalf("pull on the other instance = %d %+v %v", recorder.Code, page, err)
	}
	if !bytes.Equal(page.Messages[0].PrivateKeyPEM, request.Bundle.PrivateKeyPEM) {
		t.Fatal("the other instance did not deliver the staged key")
	}
	if status := trk35HandleDistributionAck(t, second, identity, trk35DistributionAck(page.Messages[0])); status != http.StatusNoContent {
		t.Fatalf("ack on the other instance = %d", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := first.wait(ctx, distributionKey("stage", request)); err != nil {
		t.Fatalf("the staging instance did not see the other instance's ack: %v", err)
	}
}
