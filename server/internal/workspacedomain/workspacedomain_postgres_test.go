package workspacedomain

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run workspace domain PostgreSQL tests")
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
	return database
}

// The whole user path on PostgreSQL: add, DNS records, verification by the _acme-challenge CNAME, use by a
// tunnel hostname, removal refused while used, team roles, and takeover of an unverified claim.
func TestWorkspaceDomainLifecycleOnPostgres(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, member, stranger := "usr_wsd_owner_"+suffix, "usr_wsd_member_"+suffix, "usr_wsd_stranger_"+suffix
	team := "wsd-team-" + suffix[len(suffix)-8:]
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []string{owner, member, stranger} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, team, owner, member)

	cfg := Config{ChallengeZone: "challenges.example.test", RoutingBase: "tunnels.example.test"}
	service := New(database, cfg)
	personal := workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
	teamCtx := workspace.WithContext(ctx, workspace.Scope{ID: team, TeamID: team})
	hostname := "dev-" + suffix + ".example.com"

	for _, bad := range []string{"*.example.com", "com", "co.uk", "10.0.0.1", "a..example.com", "tunnels.example.test", "x.challenges.example.test"} {
		if _, err := service.Add(personal, owner, owner, bad); !errors.Is(err, ErrInvalidHostname) && !errors.Is(err, ErrReserved) {
			t.Fatalf("Add(%q) = %v, want invalid or reserved", bad, err)
		}
	}
	view, err := service.Add(personal, owner, owner, "  HTTPS://"+strings.ToUpper(hostname)+". ")
	if err != nil {
		t.Fatal(err)
	}
	challenge, _ := tunnelcert.WorkspaceChallengeTarget("account:"+owner, hostname, cfg.ChallengeZone)
	if view.Hostname != hostname || view.State != "pending_dns" || len(view.Records) != 3 ||
		view.Records[0] != (Record{Name: "_acme-challenge." + hostname, Type: "CNAME", Value: challenge, Purpose: "verification", Required: true}) ||
		view.Records[1].Name != "*."+hostname || view.Records[1].Value != RoutingName(view.ID, cfg.RoutingBase) || view.Records[1].Type != "CNAME" {
		t.Fatalf("added view = %+v", view)
	}
	if again, err := service.Add(personal, owner, owner, hostname); err != nil || again.ID != view.ID {
		t.Fatalf("adding it again = %+v %v, want the same domain", again, err)
	}

	// Another workspace may take over a claim nobody verified, and the owner takes it back: same target.
	if taken, err := service.Add(personal, stranger, stranger, hostname); err != nil || taken.ID == view.ID {
		t.Fatalf("takeover of an unverified claim = %+v %v", taken, err)
	}
	if view, err = service.Add(personal, owner, owner, hostname); err != nil || view.Records[0].Value != challenge {
		t.Fatalf("owner re-add = %+v %v", view, err)
	}

	// DNS: missing, wrong, then the exact CNAME verifies.
	answers := map[string]string{}
	worker, err := NewWorker(database, cfg, WorkerConfig{Lookup: func(_ context.Context, name string) (string, error) { return answers[name], nil }})
	if err != nil {
		t.Fatal(err)
	}
	check := func(want string) View {
		t.Helper()
		if _, err := service.Check(personal, owner, view.ID); err != nil {
			t.Fatal(err)
		}
		if err := worker.Run(ctx, 500); err != nil {
			t.Fatal(err)
		}
		got, err := service.Get(personal, owner, view.ID)
		if err != nil || got.State != want {
			t.Fatalf("state = %+v %v, want %s", got, err, want)
		}
		return got
	}
	if got := check("pending_dns"); got.DNSFailure == nil || got.DNSFailure.Code != "dns_not_found" {
		t.Fatalf("missing record failure = %+v", got.DNSFailure)
	}
	answers["_acme-challenge."+hostname] = "elsewhere.example.net"
	if got := check("pending_dns"); got.DNSFailure == nil || got.DNSFailure.Code != "wrong_record" {
		t.Fatalf("wrong record failure = %+v", got.DNSFailure)
	}
	answers["_acme-challenge."+hostname] = challenge
	if got := check("verified"); got.DNSFailure != nil || got.VerifiedAt == nil {
		t.Fatalf("verified view = %+v", got)
	}
	if _, err := service.Add(personal, stranger, stranger, hostname); !errors.Is(err, ErrConflict) {
		t.Fatalf("claiming a verified domain = %v, want ErrConflict", err)
	}
	if _, err := worker.WorkspaceCertificate(ctx, owner, view.ID); !errors.Is(err, tunnelcert.ErrWorkspaceCertificatePending) {
		t.Fatalf("certificate before issuance = %v", err)
	}
	// Issuance: one order for the domain and its wildcard, sealed, then served to hostnames under it.
	issuer := &fakeIssuer{}
	worker.wc.Issuer, worker.wc.Keys, worker.wc.MasterKeyReference, worker.wc.OwnerID = issuer, tunnelcert.ReferenceKeySource{Keys: map[string][]byte{"master/test": make([]byte, 32)}}, "master/test", "wsd-test"
	if err := worker.Run(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if got, _ := service.Get(personal, owner, view.ID); got.Certificate.State != "ready" || got.Certificate.ExpiresAt == nil ||
		len(issuer.names) != 2 || issuer.names[0] != "*."+hostname || issuer.names[1] != hostname || issuer.domain.TargetKind != tunnelcert.TargetWorkspaceDomain {
		t.Fatalf("certificate = %+v (%+v), order %v %+v", got.Certificate, got.Certificate.Failure, issuer.names, issuer.domain)
	}
	bundle, err := worker.WorkspaceCertificate(ctx, owner, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Validate("app."+hostname, time.Now(), 0); err != nil {
		t.Fatalf("served certificate does not cover a hostname under the domain: %v", err)
	}
	if err := worker.Run(ctx, 500); err != nil || len(issuer.names) != 2 {
		t.Fatalf("a fresh certificate was ordered again: %v (%d names)", err, len(issuer.names))
	}

	// Covering: the domain, one label under it and its wildcard; nothing deeper, nothing of another workspace.
	err = database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		for host, want := range map[string]error{hostname: nil, "app." + hostname: nil, "*." + hostname: nil, "a.b." + hostname: ErrNotCovered, "*.a." + hostname: ErrNotCovered} {
			if _, err := Covering(ctx, tx, owner, teamArg(workspace.Scope{}), host); !errors.Is(err, want) {
				return fmt.Errorf("Covering(%q) = %v, want %v", host, err, want)
			}
		}
		if _, err := Covering(ctx, tx, stranger, teamArg(workspace.Scope{}), "app."+hostname); !errors.Is(err, ErrNotCovered) {
			return fmt.Errorf("another account covered = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// In use: removal is refused and names the use.
	tunnel := "tun_wsd_" + suffix
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,'linux','amd64','/w','offline','released')`, "mch_wsd_"+suffix, owner)
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,'web',$3,$4,$5,$2)`, tunnel, owner, testutil.EndpointID(tunnel), "https://"+testutil.EndpointLabel(testutil.EndpointID(tunnel))+".example.test", "mch_wsd_"+suffix)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,'web','http','managed','m.example.test','http','127.0.0.1:3000',$3,$3)`, "rte_wsd_"+suffix, tunnel, owner)
	exec(`INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,workspace_domain_id) VALUES($1,$2,$3,$4,$5,'exact','x','verified','t',$6)`,
		"dom_wsd_"+suffix, owner, tunnel, "rte_wsd_"+suffix, "app."+hostname, view.ID)
	var inUse *InUseError
	if err := service.Remove(personal, owner, view.ID); !errors.As(err, &inUse) || len(inUse.Uses) != 1 || inUse.Uses[0].Name != "web" || inUse.Uses[0].Hostname != "app."+hostname {
		t.Fatalf("removing a used domain = %v %+v", err, inUse)
	}
	if listed, err := service.List(personal, owner); err != nil || len(listed) != 1 || len(listed[0].UsedBy) != 1 {
		t.Fatalf("list = %+v %v", listed, err)
	}
	exec(`UPDATE paperboat.tunnel_domains SET deleted_at=now(),ownership_state='revoked',conflict_state='quarantined',quarantine_until=now()+interval '1 day' WHERE id=$1`, "dom_wsd_"+suffix)
	if err := service.Remove(personal, owner, view.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(personal, owner, view.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed domain = %v", err)
	}

	// Teams: members see team domains, only owners and admins add or remove them.
	teamHost := "team-" + suffix + ".example.com"
	if _, err := service.Add(teamCtx, member, member, teamHost); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member add = %v, want ErrForbidden", err)
	}
	added, err := service.Add(teamCtx, owner, owner, teamHost)
	if err != nil || added.Workspace != team {
		t.Fatalf("owner add = %+v %v", added, err)
	}
	if listed, err := service.List(teamCtx, member); err != nil || len(listed) != 1 {
		t.Fatalf("member list = %+v %v", listed, err)
	}
	if listed, err := service.List(personal, member); err != nil || len(listed) != 0 {
		t.Fatalf("member personal list shows team domains = %+v %v", listed, err)
	}
	if err := service.Remove(teamCtx, member, added.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member remove = %v, want ErrForbidden", err)
	}
	exec(`UPDATE paperboat.team_members SET role='admin' WHERE team_id=$1 AND account_id=$2`, team, member)
	if err := service.Remove(teamCtx, member, added.ID); err != nil {
		t.Fatalf("admin remove = %v", err)
	}
	if _, err := service.List(teamCtx, stranger); err == nil {
		t.Fatal("a non-member listed team domains")
	}
}

type fakeIssuer struct {
	domain tunnelcert.Domain
	names  []string
}

func (f *fakeIssuer) IssueDNS01Names(_ context.Context, domain tunnelcert.Domain, names []string) (tunnelcert.CertificateBundle, error) {
	f.domain, f.names = domain, append(f.names, names...)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tunnelcert.CertificateBundle{}, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(90 * 24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tunnelcert.CertificateBundle{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tunnelcert.CertificateBundle{}, err
	}
	return tunnelcert.CertificateBundle{CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}, nil
}
