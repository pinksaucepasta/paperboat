package httpapi

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
)

type machineProofErrorVerifier struct {
	claims controlplane.MachineRequestClaims
	err    error
}

func (v machineProofErrorVerifier) VerifyMachineRequest(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error) {
	return v.claims, v.err
}

func TestPreviewLeasePreconditionKeepsStorageFailuresDistinctFromNotFound(t *testing.T) {
	if got := previewLeasePreconditionError(previewtunnelstore.ErrNotFound); !errors.Is(got, previewattachment.ErrNotFound) {
		t.Fatalf("missing lease mapped to %v", got)
	}
	cause := syscall.ECONNRESET
	got := previewLeasePreconditionError(cause)
	if errors.Is(got, previewattachment.ErrNotFound) || !errors.Is(got, cause) {
		t.Fatalf("storage failure mapping lost cause or became not-found: %v", got)
	}
}

func TestPreviewAttachmentAdapterPreservesVerifierOutageCause(t *testing.T) {
	cause := syscall.ECONNRESET
	verifier, err := NewPreviewAttachmentMachineProofVerifier(machineProofErrorVerifier{err: cause})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/previews/preview_1/attachment", nil)
	request.Header.Set("Authorization", "Bearer machine_credential")
	request.Header.Set("X-Paperboat-Machine-Identity", "machine_credential")
	request.Header.Set("X-Paperboat-Machine-Proof", "cHJvb2Y")
	_, err = verifier.VerifyMachineRequest(context.Background(), request, []byte(`{}`))
	if !errors.Is(err, cause) || errors.Is(err, previewattachment.ErrUnauthorized) {
		t.Fatalf("adapter error=%v; want original verifier outage without expected-rejection classification", err)
	}
}

func TestPreviewAttachmentPublicProjectionOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	account, preview, domain := "account_projection_"+suffix, "preview_projection_"+suffix, "domain_projection_"+suffix
	now := time.Now().UTC()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, []any{account, suffix + "@example.test"}},
		{`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode) VALUES('account',$1,'tunnel','self-hosted-only')`, []any{account}},
		{`INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline,created_at) VALUES('foreground',$1,'https://logical.preview.example.test',$2,$2,'machine_projection','session_projection','http','127.0.0.1:3000','public',$3,$4)`, []any{preview, account, now.Add(time.Hour), now}},
		{`INSERT INTO paperboat.preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,1,'customer.example.test','exact','challenge_projection','verified','edge.example.test','managed','issuing','ready','clear')`, []any{domain, account, preview}},
	} {
		if _, err := database.SQL().ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM paperboat.tunnel_certificate_records WHERE account_id=$1`, `DELETE FROM paperboat.preview_domains WHERE account_id=$1`, `DELETE FROM paperboat.preview_leases WHERE account_id=$1`, `DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`, `DELETE FROM paperboat.users WHERE id=$1`} {
			if _, err := database.SQL().ExecContext(ctx, q, account); err != nil {
				t.Error(err)
			}
		}
	})
	store, err := previewtunnelstore.New(database)
	if err != nil {
		t.Fatal(err)
	}
	checker, _ := NewPreviewAttachmentLeasePreconditionChecker(store)
	attachment := previewattachment.Attachment{Binding: previewattachment.Binding{AccountID: account, PreviewID: preview, OperationID: "operation_projection", OwnerMachineID: "machine_projection", OwnerSessionID: "session_projection", LeaseGeneration: 1}, Endpoint: "https://logical.preview.example.test", Target: previewattachment.Target{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public"}
	proof := previewattachment.MachineProof{UserID: account, MachineID: attachment.OwnerMachineID, OperationID: attachment.OperationID}
	got, err := checker.ProjectPreviewAttachmentEndpoint(ctx, proof, attachment)
	if err != nil || got.Endpoint != attachment.Endpoint {
		t.Fatalf("pending transport identity changed: %v", err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_domains SET certificate_state='ready',certificate_reference='certificate_projection' WHERE id=$1`, domain); err != nil {
		t.Fatal(err)
	}
	got, err = checker.ProjectPreviewAttachmentEndpoint(ctx, proof, attachment)
	if err != nil || got.Endpoint != "https://customer.example.test" || attachment.Endpoint != "https://logical.preview.example.test" {
		t.Fatalf("ready customer projection missing or mutated activation identity: %v", err)
	}
	var stored string
	if err := database.SQL().QueryRowContext(ctx, `SELECT endpoint FROM paperboat.preview_leases WHERE id=$1`, preview).Scan(&stored); err != nil || stored != attachment.Endpoint {
		t.Fatal("activation identity changed", err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET generation=2 WHERE id=$1;`, preview); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_domains SET preview_generation=2 WHERE id=$1`, domain); err != nil {
		t.Fatal(err)
	}
	got, err = checker.ProjectPreviewAttachmentEndpoint(ctx, proof, attachment)
	if err != nil || got.Endpoint != "https://customer.example.test" {
		t.Fatal("renewal lost projection", err)
	}

	// Readiness advances the same immutable publication authority as renewal.
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_certificate_records(id,domain_id,account_id,user_account_id,hostname,domain_generation,certificate_generation,strategy,state,certificate_reference,master_key_reference,certificate_ciphertext,private_key_ciphertext,fingerprint,issuer,not_before,expires_at,renewal_at,target_kind,preview_id,preview_generation,preview_state,preview_expires_at) VALUES($1,$2,$3,$3,'customer.example.test',1,1,'delegated_dns01','active','certificate_projection','master_projection',decode(repeat('00',29),'hex'),decode(repeat('00',29),'hex'),decode(repeat('00',32),'hex'),'fixture',$4,$5,$6,'preview_lease',$7,2,'active',$8)`, "certificate_"+suffix, domain, account, now.Add(-time.Minute), now.Add(24*time.Hour), now.Add(12*time.Hour), preview, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ready, err := store.MarkPreviewLeaseReadyV1(ctx, previewtunnelstore.MarkPreviewLeaseReadyV1Input{AuditEventID: "audit_ready_" + suffix, AccountID: account, ActorID: account, ActorType: "user", PreviewID: preview, ExpectedGeneration: 2, AllocationState: "ready", EdgeState: "ready", OriginState: "ready", CorrelationID: "correlation_ready", RequestID: "request_ready", SourceMachineID: "machine_projection", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if ready.Generation != 3 || ready.PublicEndpoint == nil || *ready.PublicEndpoint != "https://customer.example.test" || ready.AllocationState != "ready" || ready.EdgeState != "ready" || ready.OriginState != "ready" {
		t.Fatalf("readiness lost canonical publication: %+v", ready)
	}
	var domainGeneration, certificateGeneration int64
	if err := database.SQL().QueryRowContext(ctx, `SELECT d.preview_generation,c.preview_generation FROM paperboat.preview_domains d JOIN paperboat.tunnel_certificate_records c ON c.domain_id=d.id WHERE d.id=$1`, domain).Scan(&domainGeneration, &certificateGeneration); err != nil {
		t.Fatal(err)
	}
	if domainGeneration != ready.Generation || certificateGeneration != ready.Generation {
		t.Fatalf("readiness lost exact authority generations domain=%d certificate=%d lease=%d", domainGeneration, certificateGeneration, ready.Generation)
	}
	got, err = checker.ProjectPreviewAttachmentEndpoint(ctx, proof, attachment)
	if err != nil || got.Endpoint != "https://customer.example.test" {
		t.Fatal("readiness API response lost customer endpoint", err)
	}
	proof.MachineID = "other_machine"
	if _, err := checker.ProjectPreviewAttachmentEndpoint(ctx, proof, attachment); !errors.Is(err, previewattachment.ErrUnauthorized) {
		t.Fatal("different machine accepted", err)
	}
}
