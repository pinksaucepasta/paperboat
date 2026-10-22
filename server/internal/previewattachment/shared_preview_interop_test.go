package previewattachment_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat-server/internal/teams/teamstest"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat-server/internal/previewdispatch"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat-server/internal/previewv1"
)

type sharedPreviewRoute struct{ route previewdispatch.MachineRoute }

func (r sharedPreviewRoute) ResolvePreviewDispatchRoute(context.Context, string, string) (previewdispatch.MachineRoute, error) {
	return r.route, nil
}

func TestSharedPreviewSignedLifecycleOnPostgres(t *testing.T) {
	f := newSharedPreviewFixture(t)
	ctx := context.Background()
	owner, machine := f.addAccount(t, "preview_issuer")
	member, _ := f.addAccount(t, "preview_member")
	team := "preview_team_" + f.suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	t.Cleanup(func() { exec(`DELETE FROM paperboat.teams WHERE team_id=$1`, team) })
	// The production shared-preview authorization function requires paid team state.
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,now()-interval '1 hour',now()+interval '1 hour')`, "shared-preview-billing-"+f.suffix, team)
	t.Cleanup(func() { exec(`DELETE FROM paperboat.billing_workspaces WHERE team_id=$1`, team) })
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, member)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, team, machine, owner)
	teamstest.SetMachineAccess(t, f.database, team, machine, "", "preview_manage")
	exec(`UPDATE paperboat.user_machines SET online=true,installation_generation=1,observed_capabilities=ARRAY['preview_launch'] WHERE id=$1`, machine)
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	var dispatch previewv1.DispatchRequest
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if err = json.NewDecoder(r.Body).Decode(&dispatch); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(previewv1.DispatchOutcome{Schema: previewv1.Schema, Kind: previewv1.PreviewDispatchKind, PreviewID: dispatch.PreviewID, OperationID: dispatch.OperationID, State: "accepted", Generation: dispatch.ExpectedGeneration})
	}))
	defer target.Close()
	dispatcher, err := previewdispatch.New(previewdispatch.Config{Resolver: sharedPreviewRoute{previewdispatch.MachineRoute{Shared: true, EnvironmentID: "env_" + machine, BaseURL: target.URL}}, Signer: signer, Issuer: "shared-preview-test", Client: target.Client()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := previewtunnelstore.New(f.database)
	if err != nil {
		t.Fatal(err)
	}
	service, err := previewv1.NewService(store, previewv1.Config{EndpointDomain: "preview.example.test", CursorKey: make([]byte, 32), Dispatcher: dispatcher})
	if err != nil {
		t.Fatal(err)
	}
	actor := previewtunnelapi.RequestContext{Actor: previewtunnelapi.Actor{Role: "user"}, RequestID: "shared-preview", CorrelationID: "shared-preview"}
	actor.Actor.AccountID = member
	actor.Actor.ActorID = member
	actor.Actor.Scopes = []string{"previews:read", "previews:write"}
	created, err := service.Create(ctx, actor, previewv1.CreateRequest{OwnerMachineID: machine, OwnerSessionID: "shared_session_" + f.suffix, OwnerSessionKind: "foreground", Target: previewv1.Target{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "private", IdempotencyKey: "shared-create"})
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.AccountID != member || dispatch.ActorID != member || dispatch.OwnerMachineID != machine || token == "" {
		t.Fatal("signed dispatch lost requester or target")
	}
	resolver, err := previewattachment.NewDBPreviewLeaseResolver(f.database)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.OwnerSessionKind != "foreground" || created.Preview.OwnerSessionKind != "foreground" {
		t.Fatal("shared preview owner lifecycle changed before dispatch")
	}
	lookup := previewattachment.LeaseLookupRequest{UserID: owner, MachineID: machine, InstallationGeneration: 1, PreviewID: dispatch.PreviewID, OperationID: dispatch.OperationID, OwnerSessionID: dispatch.OwnerSessionID}
	resolved, err := resolver.ResolvePreviewLease(ctx, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.MachineAccountID != owner || resolved.AccountID != member {
		t.Fatal("attachment identity conflated issuer and resource owner")
	}
	// The second attachment of the preview is proven through the same lease and gets its own
	// operation, registered for the lease, so the attachment table accepts it.
	secondLookup := lookup
	secondLookup.OperationID = dispatch.OperationID + previewattachment.SecondaryOperationSuffix
	second, err := resolver.ResolvePreviewLease(ctx, secondLookup)
	if err != nil || second.OperationID != secondLookup.OperationID || second.PreviewID != resolved.PreviewID || second.AccountID != resolved.AccountID {
		t.Fatalf("second attachment lease = %+v, %v", second, err)
	}
	var registered, completed int
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.preview_lease_create_operations WHERE account_id=$1 AND preview_id=$2 AND operation_id=$3 AND slot=1),(SELECT count(*) FROM paperboat.operations WHERE account_id=$1 AND id=$3 AND state='succeeded' AND operation_type='preview.attachment_secondary')`, second.AccountID, second.PreviewID, second.OperationID).Scan(&registered, &completed); err != nil || registered != 1 || completed != 1 {
		t.Fatalf("second attachment operation registered=%d completed=%d err=%v", registered, completed, err)
	}
	if _, err := resolver.ResolvePreviewLease(ctx, secondLookup); err != nil {
		t.Fatalf("resolving the second attachment again: %v", err)
	}
	machineRequest := actor
	machineRequest.Actor.AccountID = owner
	machineRequest.Actor.ActorID = owner
	machineRequest.Actor.Role = "user"
	machineRequest.Actor.HostID = machine
	machineRequest.Actor.MachineID = machine
	renewed, err := service.Renew(ctx, machineRequest, dispatch.PreviewID, previewv1.MutationRequest{ExpectedGeneration: dispatch.ExpectedGeneration, OwnerSessionID: dispatch.OwnerSessionID, IdempotencyKey: "shared-renew"})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Preview.AccountID != member {
		t.Fatal("renewal changed resource owner")
	}
	teamstest.SetMachineAccess(t, f.database, team, machine, "")
	if _, err = resolver.ResolvePreviewLease(ctx, lookup); !errors.Is(err, previewattachment.ErrUnauthorized) {
		t.Fatalf("revoked attachment: %v", err)
	}
	if renewed.Preview.OwnerSessionKind != "foreground" {
		t.Fatal("owner lifecycle kind changed on renewal")
	}
	if _, err = service.Renew(ctx, machineRequest, dispatch.PreviewID, previewv1.MutationRequest{ExpectedGeneration: dispatch.ExpectedGeneration + 1, OwnerSessionID: dispatch.OwnerSessionID, IdempotencyKey: "revoked-renew"}); err == nil {
		t.Fatal("renewed after revocation")
	}
	stopped, err := service.Stop(ctx, machineRequest, dispatch.PreviewID, previewv1.MutationRequest{ExpectedGeneration: dispatch.ExpectedGeneration + 1, IdempotencyKey: "shared-stop"})
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("PAPERBOAT_SHARED_PREVIEW_FIXTURE"); path != "" {
		pub, err := signer.ActivePublicKeyPEM()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(struct {
			Issuer, MachineIssuer, PublicKey, Token string
			Dispatch                                previewv1.DispatchRequest
			Created                                 previewv1.CreateResult
			Renewed                                 previewv1.RenewResult
			Stopped                                 previewv1.StopResult
		}{"shared-preview-test", owner, pub, token, dispatch, created, renewed, stopped})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

type sharedPreviewFixture struct {
	database *db.DB
	suffix   string
}

func newSharedPreviewFixture(t *testing.T) *sharedPreviewFixture {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return &sharedPreviewFixture{database: database, suffix: fmt.Sprint(time.Now().UnixNano())}
}
func (f *sharedPreviewFixture) addAccount(t *testing.T, label string) (string, string) {
	account, machine := "sp_"+label+f.suffix, "sp_machine_"+label+f.suffix
	ctx := context.Background()
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status)VALUES($1,$1,$1||'@invalid.test','active')`, account); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(machine))
	key := base64.RawURLEncoding.EncodeToString(hash[:])
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,public_identity_key,configured_capabilities) VALUES ($1,$2,$1,$1,'linux','amd64','/workspace','online','occupied',$3,ARRAY['preview_launch'])`, machine, account, key); err != nil {
		t.Fatal(err)
	}
	// The isolated database owner removes immutable audit fixtures at teardown.
	return account, machine
}
