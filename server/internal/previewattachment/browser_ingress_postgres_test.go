package previewattachment_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat/server/internal/browseringress"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// This is live SQL composition evidence. Connector cryptography and browser
// provider login are exercised at their separate connected boundaries.
func TestBrowserPreviewIngressPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if err = db.Migrate(ctx, database); err != nil {
			t.Fatal(err)
		}
	}
	f, err := previewattachment.NewBrowserAccountingFixture(ctx, database, fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM usage_accounting_authorities WHERE environment_id=$1`, f.EnvironmentID)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM billing_workspaces WHERE user_id=$1`, f.AccountID)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM sessions WHERE user_id=$1`, f.AccountID)
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM preview_leases WHERE id=$1`, []any{f.PreviewID(1)}},
			{`DELETE FROM teams WHERE team_id=$1`, []any{f.SecondPreviewTeam}},
		} {
			if _, e := database.SQL().ExecContext(context.Background(), statement.query, statement.args...); e != nil {
				t.Error(e)
			}
		}
		if _, e := database.SQL().ExecContext(context.Background(), `DELETE FROM users WHERE id=$1`, f.AccountID); e != nil {
			t.Error(e)
		}
		if _, e := database.SQL().ExecContext(context.Background(), `DELETE FROM control_tunnel_nodes WHERE id=$1`, f.NodeID); e != nil {
			t.Error(e)
		}
	})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := database.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`UPDATE preview_leases SET access_mode='private' WHERE id=$1`, f.PreviewID(0))
	exec(`INSERT INTO machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) VALUES($1,$2,1,$1,$3,$4,1)`, "renew_"+f.Suffix, f.MachineID, now.Add(-time.Minute), now.Add(time.Hour))
	exec(`INSERT INTO machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) SELECT machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at FROM machine_control_renewals WHERE operation_id=$1`, "renew_"+f.Suffix)
	repo, _ := previewattachment.NewSQLRepository(database)
	attachment := f.Attachment(f.PreviewID(0), f.OperationID(0), f.OwnerSessionID)
	attachment.Endpoint = fmt.Sprintf("https://carrier-epoch-%s-a.preview.example.test", f.Suffix)
	attachment.AccessMode = "private"
	if _, err = repo.CreatePending(ctx, attachment); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE preview_lease_carrier_attachments SET state='ready',edge_ready=true,origin_ready=true,ready_at=now() WHERE account_id=$1 AND preview_id=$2`, f.AccountID, f.PreviewID(0))
	session := "ses_browser_" + f.Suffix
	exec(`INSERT INTO sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, session, f.AccountID, "fixture_hash_"+f.Suffix, "csrf_"+f.Suffix, now.Add(time.Hour))
	access, err := browseraccess.NewService(database, "https://login.example.test")
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(attachment.Endpoint, "https://")
	begin, err := access.Begin(ctx, browseraccess.BeginRequest{Host: host, ReturnPath: "/app"})
	if err != nil {
		t.Fatal(err)
	}
	issue, err := access.Issue(ctx, browseraccess.IssueRequest{TransactionID: begin.TransactionID, Principal: auth.Session{ID: session, UserID: f.AccountID}})
	if err != nil {
		t.Fatal(err)
	}
	login, err := access.Redeem(ctx, browseraccess.RedeemRequest{TransactionID: begin.TransactionID, State: begin.State, Handoff: issue.Handoff, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	a, err := access.Authorize(ctx, browseraccess.AuthorizeRequest{Token: login.Token, Host: host, ResourceKind: "preview", ResourceID: attachment.PreviewID, RouteID: attachment.RouteID})
	if err != nil {
		t.Fatal(err)
	}
	ingress := browseringress.Service{DB: database, Access: access}
	d, err := ingress.Resolve(ctx, a, f.NodeID, f.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: attachment.AccountID, TunnelID: attachment.TunnelID, ConnectorID: attachment.ConnectorID, SessionID: attachment.SessionID, ProcessGeneration: attachment.ProcessGeneration, Generation: attachment.ConfigGeneration, RouteID: attachment.RouteID, RequestID: "browser-request", Kind: "http_browser"}
	if d.Authorize(d, open, f.NodeID, f.Epoch, time.Now().UTC()) != nil || d.Binding.PublicationID != attachment.PreviewID || d.Binding.OriginAddress != "127.0.0.1:3000" {
		t.Fatal("incorrect complete browser binding")
	}
	assertTeamBrowserPreviewAccounting(t, database, f, repo, access, now)
	// Heartbeat revision does not change the viewer's authority dimensions.
	exec(`UPDATE preview_leases SET generation=generation+1,lease_deadline=lease_deadline+interval '1 minute' WHERE id=$1`, attachment.PreviewID)
	exec(`UPDATE preview_lease_carrier_attachments SET lease_generation=lease_generation+1,expires_at=expires_at+interval '1 minute' WHERE preview_id=$1`, attachment.PreviewID)
	a, err = access.AuthorizeGrant(ctx, d.GrantID, host, "preview", attachment.PreviewID, attachment.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ingress.Resolve(ctx, a, f.NodeID, f.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	d.IssuedAt, d.ExpiresAt = next.IssuedAt, next.ExpiresAt
	if d.Authorize(next, open, f.NodeID, f.Epoch, time.Now().UTC()) != nil {
		t.Fatal("heartbeat changed browser authority")
	}
	exec(`UPDATE preview_leases SET target_address='127.0.0.1:3999' WHERE id=$1`, attachment.PreviewID)
	if _, err = access.AuthorizeGrant(ctx, d.GrantID, host, "preview", attachment.PreviewID, attachment.RouteID); err == nil {
		t.Fatal("changed preview target retained browser authority")
	}
	exec(`UPDATE preview_leases SET target_address='127.0.0.1:3000' WHERE id=$1`, attachment.PreviewID)
	exec(`UPDATE user_machines SET revoked_at=now() WHERE id=$1`, f.MachineID)
	if _, err = ingress.Resolve(ctx, a, f.NodeID, f.Epoch); !errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("revoked machine ingress error=%v, want expected denial", err)
	}
	exec(`UPDATE user_machines SET revoked_at=NULL WHERE id=$1`, f.MachineID)
	if _, err = ingress.Resolve(ctx, a, f.NodeID, f.EpochTwo); !errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("wrong edge epoch ingress error=%v, want expected denial", err)
	}
	exec(`UPDATE sessions SET revoked_at=now() WHERE id=$1`, session)
	if _, err = access.AuthorizeGrant(ctx, d.GrantID, host, "preview", attachment.PreviewID, attachment.RouteID); err == nil {
		t.Fatal("revoked login retained viewer authority")
	}
}
