package tunnelcert_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

type previewRenewalIssuer struct {
	bundle tunnelcert.CertificateBundle
	issue  func(context.Context) error
	calls  int
}

func (i *previewRenewalIssuer) Issue(ctx context.Context, _ tunnelcert.IssueRequest) (tunnelcert.CertificateBundle, error) {
	i.calls++
	if err := i.issue(ctx); err != nil {
		return tunnelcert.CertificateBundle{}, err
	}
	value := i.bundle
	value.CertificatePEM = append([]byte(nil), value.CertificatePEM...)
	value.PrivateKeyPEM = append([]byte(nil), value.PrivateKeyPEM...)
	return value, nil
}

type previewRenewalDistributor struct {
	*tunnelcert.MemoryDistributor
	wait func(context.Context) error
}

func (d *previewRenewalDistributor) WaitReady(ctx context.Context, r tunnelcert.DistributionRequest) error {
	if d.wait != nil {
		return d.wait(ctx)
	}
	return d.MemoryDistributor.WaitReady(ctx, r)
}

func TestPreviewCertificateIssuanceRenewalFencesOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("requires isolated PAPERBOAT_TEST_DATABASE_DSN")
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
	for _, kind := range []string{"renewal", "distribution_renewal", "owner_mismatch", "domain_change", "expiry", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			f := newPlatformAcceptanceFixture(fmt.Sprintf("%d", time.Now().UnixNano()))
			for _, setup := range []struct {
				query string
				args  []any
			}{
				{`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$2,$3,'active')`, []any{f.previewAccountID, "subject-renewal-" + f.suffix, "renewal-" + f.suffix + "@example.test"}},
				{`INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline,user_deadline,created_at) VALUES('foreground',$1,$2,$3,$3,$4,$5,'http','127.0.0.1:3000','public',$6,$7,$8)`, []any{f.previewLeaseID, "https://preview-" + f.suffix + ".preview.example.test", f.previewAccountID, "machine-" + f.suffix, "session-" + f.suffix, now.Add(24 * time.Hour), now.Add(12 * time.Hour), now}},
				{`INSERT INTO paperboat.preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,1,$4,'exact','dns-preview-platform','verified','edge.example.test','managed','issuing','ready','clear')`, []any{f.previewDomainID, f.previewAccountID, f.previewLeaseID, f.previewHostname}},
			} {
				if _, err := database.SQL().ExecContext(context.Background(), setup.query, setup.args...); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				ctx := context.Background()
				for _, query := range []string{
					`DELETE FROM paperboat.tunnel_certificate_records WHERE domain_id=$1`,
					`DELETE FROM paperboat.tunnel_certificate_issuance_locks WHERE domain_id=$1`,
					`DELETE FROM paperboat.preview_domains WHERE id=$1`,
				} {
					if _, err := database.SQL().ExecContext(ctx, query, f.previewDomainID); err != nil {
						t.Error(err)
					}
				}
				if _, err := database.SQL().ExecContext(ctx, `DELETE FROM paperboat.preview_leases WHERE id=$1`, f.previewLeaseID); err != nil {
					t.Error(err)
				}
				if _, err := database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, f.previewAccountID); err != nil {
					t.Error(err)
				}
			})
			store, err := tunnelcert.NewSQLStore(database)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			renew := func(ctx context.Context) error {
				return database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
					if _, err := tx.Exec(ctx, `UPDATE paperboat.preview_leases SET generation=generation+1,lease_deadline=$2 WHERE id=$1`, f.previewLeaseID, now.Add(25*time.Hour)); err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, `UPDATE paperboat.preview_domains d SET preview_generation=p.generation FROM paperboat.preview_leases p WHERE d.id=$1 AND p.id=d.preview_id`, f.previewDomainID); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, `UPDATE paperboat.tunnel_certificate_records c SET preview_generation=p.generation FROM paperboat.preview_leases p WHERE c.domain_id=$1 AND p.id=c.preview_id AND c.state IN ('staged','active')`, f.previewDomainID)
					return err
				})
			}
			bundle, err := acceptanceBundle(f.previewHostname, now, 71, "preview-renewal-ca")
			if err != nil {
				t.Fatal(err)
			}
			issuer := &previewRenewalIssuer{bundle: bundle, issue: func(ctx context.Context) error {
				if kind == "cancellation" {
					cancel()
					return nil
				}
				if err := renew(ctx); err != nil {
					return err
				}
				switch kind {
				case "domain_change":
					_, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_domains SET generation=generation+1 WHERE id=$1`, f.previewDomainID)
				case "expiry":
					_, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET created_at=$2::timestamptz-interval '1 second',lease_deadline=$2 WHERE id=$1`, f.previewLeaseID, now.Add(-time.Second))
				}
				return err
			}}
			distributor := &previewRenewalDistributor{MemoryDistributor: tunnelcert.NewMemoryDistributor()}
			if kind == "distribution_renewal" {
				distributor.wait = func(ctx context.Context) error {
					if err := renew(ctx); err != nil {
						return err
					}
					return tunnelcert.ErrGenerationConflict
				}
			}
			coordinator, err := tunnelcert.NewCoordinator(tunnelcert.Config{Store: store, Locks: tunnelcert.NewMemoryLock(), Keys: tunnelcert.ReferenceKeySource{Keys: map[string][]byte{"master/current": bytes.Repeat([]byte{0x42}, 32)}}, Issuer: issuer, CAA: tunnelcert.MemoryCAA{Result: tunnelcert.CAAResult{State: "ready"}}, Delegated: tunnelcert.MemoryDelegator{Challenge: tunnelcert.DNSChallenge{Name: "_acme-challenge." + f.previewHostname, Type: "TXT", Value: "bounded-test-challenge", TTL: time.Minute}}, Distributor: distributor, MasterKeyReference: "master/current", OwnerID: "preview-renewal-worker", Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			captured := tunnelcert.Domain{ID: f.previewDomainID, AccountID: f.previewAccountID, TargetKind: tunnelcert.TargetPreviewLease, PreviewID: f.previewLeaseID, PreviewGeneration: 1, PreviewOwnerMachineID: "machine-" + f.suffix, PreviewOwnerSessionID: "session-" + f.suffix, PreviewState: "active", PreviewExpiresAt: now.Add(12 * time.Hour), Hostname: f.previewHostname, ChallengeReference: "dns-preview-platform", Generation: 1, Strategy: tunnelcert.StrategyDelegatedDNS01, OwnershipState: "verified", CAAState: "ready", RenewalDue: true, EdgeTargets: []tunnelcert.EdgeTarget{{NodeID: "edge-renewal", ProcessEpoch: "epoch-renewal", Generation: 1}}}
			if kind == "owner_mismatch" {
				captured.PreviewOwnerSessionID = "different-captured-session"
			}
			result, err := coordinator.Ensure(ctx, captured)
			if kind == "distribution_renewal" {
				if !errors.Is(err, tunnelcert.ErrCertificateNotReady) {
					t.Fatalf("distribution renewal error=%v", err)
				}
				distributor.wait = nil
				result, err = coordinator.Ensure(ctx, captured)
			}
			expectedCalls := 1
			if kind == "owner_mismatch" {
				expectedCalls = 0
			}
			if issuer.calls != expectedCalls {
				t.Fatalf("CA calls=%d, want%d", issuer.calls, expectedCalls)
			}
			if kind != "renewal" && kind != "distribution_renewal" {
				if kind == "cancellation" {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation=%v", err)
					}
				} else if !errors.Is(err, tunnelcert.ErrGenerationConflict) {
					t.Fatalf("changed authority=%v", err)
				}
				var count int
				if err := database.SQL().QueryRowContext(context.Background(), `SELECT count(*) FROM paperboat.tunnel_certificate_records WHERE domain_id=$1`, f.previewDomainID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 || len(distributor.Events) != 0 {
					t.Fatal("rejected authority persisted or distributed leaf")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			certificate, found, err := store.CurrentPreview(ctx, f.previewAccountID, f.previewDomainID, f.previewLeaseID)
			expected := uint64(2)
			if kind == "distribution_renewal" {
				expected = 3
			}
			if err != nil || !found || certificate.State != tunnelcert.StateActive || certificate.PreviewGeneration != expected || result.Certificate.PreviewGeneration != expected {
				t.Fatalf("current certificate=%+v found=%v err=%v", certificate, found, err)
			}
			refreshed, err := coordinator.RefreshPreviewIssuanceTarget(ctx, captured, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := coordinator.CommitPreviewDomainCertificateReady(ctx, refreshed.AccountID, refreshed.ID, refreshed.PreviewID, refreshed.PreviewGeneration, refreshed.Generation, certificate, now); err != nil {
				t.Fatal(err)
			}
		})
	}
}
