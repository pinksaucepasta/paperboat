package previewattachment

import (
	"context"
	"encoding/hex"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"strings"
	"time"
)

// LazyActivationFixture exposes the established attachment SQL fixture only to
// external tests, avoiding the previewv1/previewdomain production import cycle.
type LazyActivationFixture struct {
	AccountID, MachineID, EnvironmentID, NodeID, Epoch, EpochTwo, Suffix string
	SecondPreviewTeam                                                    string
	LazyPolicyID, LazyHostname, OwnerSessionID                           string
	fixture                                                              previewCarrierPostgresFixture
}

func NewLazyActivationFixture(ctx context.Context, database *db.DB, suffix string) (LazyActivationFixture, error) {
	f := previewCarrierPostgresFixture{suffix: suffix}
	if err := f.insert(ctx, database, time.Now().UTC()); err != nil {
		return LazyActivationFixture{}, err
	}
	return lazyActivationFixture(f), nil
}

// NewBrowserAccountingFixture creates the second publication in its team at
// INSERT time, preserving the same immutable payer rule as the production query.
func NewBrowserAccountingFixture(ctx context.Context, database *db.DB, suffix string) (LazyActivationFixture, error) {
	f := previewCarrierPostgresFixture{suffix: suffix, secondPreviewTeam: "browser-accounting-" + suffix}
	if err := f.insert(ctx, database, time.Now().UTC()); err != nil {
		return LazyActivationFixture{}, err
	}
	return lazyActivationFixture(f), nil
}

func NewLazyRuntimeFixture(ctx context.Context, database *db.DB, suffix, policyID, ownerSessionID string) (LazyActivationFixture, error) {
	f := previewCarrierPostgresFixture{
		suffix: suffix, ownerSessionKind: "lazy_runtime", sessionID: ownerSessionID,
		lazyPolicyID: policyID,
	}
	if err := f.insert(ctx, database, time.Now().UTC()); err != nil {
		return LazyActivationFixture{}, err
	}
	return lazyActivationFixture(f), nil
}

func lazyActivationFixture(f previewCarrierPostgresFixture) LazyActivationFixture {
	return LazyActivationFixture{
		AccountID: f.accountID, MachineID: f.machineID, EnvironmentID: f.envID,
		NodeID: f.nodeID, Epoch: f.epochOne, EpochTwo: f.epochTwo, Suffix: f.suffix,
		LazyPolicyID: f.lazyPolicyID, LazyHostname: f.lazyHostname, OwnerSessionID: f.sessionID,
		fixture: f, SecondPreviewTeam: f.secondPreviewTeam,
	}
}

func (f LazyActivationFixture) PreviewID(index int) string {
	if index < 0 || index >= len(f.fixture.previewID) {
		panic("preview fixture index out of range")
	}
	return f.fixture.previewID[index]
}

func (f LazyActivationFixture) OperationID(index int) string {
	if index < 0 || index >= len(f.fixture.operation) {
		panic("preview fixture index out of range")
	}
	return f.fixture.operation[index]
}
func (f LazyActivationFixture) Attachment(previewID, operationID, sessionID string) Attachment {
	current := f.fixture
	current.previewID[0], current.operation[0], current.sessionID = previewID, operationID, sessionID
	current.suffix += "_" + previewID
	return current.attachment(1, current.epochOne)
}

// ApplyLazyReadyFixture publishes the already-qualified remote observation in
// one SQL round trip. This avoids charging WAN fixture setup to the production
// activation deadline; real attachment admission has separate live SQL coverage.
func ApplyLazyReadyFixture(ctx context.Context, database *db.DB, a Attachment) error {
	if err := a.Validate(time.Time{}); err != nil {
		return err
	}
	hash, err := hex.DecodeString(a.RequestHash)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	insert := strings.Replace(insertAttachmentSQL, "issued_at, expires_at, created_at, updated_at, edge_slot", "issued_at, expires_at, created_at, updated_at, ready_at, edge_slot", 1)
	insert = strings.Replace(insert, "1,'pending',false,false,$27,$28,$29,$30,$31)", "1,'ready',true,true,$27,$28,$29,$30,$29,$31)", 1)
	if !strings.Contains(insert, "ready_at, edge_slot") || !strings.Contains(insert, "$29,$31)") {
		return errors.New("lazy ready fixture no longer matches the attachment insert; update it with the column list")
	}
	query := `WITH ready AS (UPDATE preview_leases SET allocation_state='ready',edge_state='ready',origin_state='ready',ready_at=now(),generation=generation+1 WHERE id=$2 AND terminal_state='active' RETURNING id) ` + insert
	_, err = database.Pool().Exec(ctx, query,
		a.AccountID, a.PreviewID, a.OperationID, a.IdempotencyKey, a.RequestID, a.CorrelationID, hash,
		a.OwnerMachineID, a.OwnerSessionID, a.HostID, a.EdgeNodeID, a.MachineIdentityPublicKey, a.MachineIdentityThumbprint,
		a.LeaseGeneration+1, a.TunnelID, a.ConnectorID, a.SessionID, a.ProcessGeneration, a.ConfigGeneration, a.ConfigContentHash,
		a.RouteID, a.RouteGeneration, a.EdgeEndpoints, a.EdgeProcessEpoch, a.EdgeCarrierServerSPKISHA256, a.EdgeCarrierServerCertificateChainPEM,
		a.IssuedAt.UTC(), a.ExpiresAt.UTC(), now, now, 0)
	return err
}
