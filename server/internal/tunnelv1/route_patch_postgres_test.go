package tunnelv1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func TestRoutePatchPersistsNameAndCanonicalRootOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account, host := f.addAccount(t, "route-patch")
	tunnel := "tun_route_patch_" + f.suffix
	f.createTunnel(t, account, host, tunnel, "patch-"+f.suffix)
	var route string
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT id FROM paperboat.tunnel_routes WHERE tunnel_id=$1 AND deleted_at IS NULL ORDER BY id LIMIT 1`, tunnel).Scan(&route); err != nil {
		t.Fatal(err)
	}
	service, err := NewResourceService(f.repository, ResourceConfig{CursorKey: bytes.Repeat([]byte("p"), 32), Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(false)
	request.Actor.AccountID, request.Actor.ActorID = account, account
	before, err := service.GetRoute(ctx, request, tunnel, route)
	if err != nil {
		t.Fatal(err)
	}
	name, path := "renamed-route", "/"
	input := RoutePatchRequest{Name: &name, PathPrefix: &path, PathPrefixSet: true, Mutation: ResourceMutationInput{ExpectedGeneration: before.Generation, IdempotencyKey: "route-patch:" + f.suffix, RequestHash: sha256.Sum256([]byte("rename-and-root:" + f.suffix))}}
	changed, err := service.PatchRoute(ctx, request, tunnel, route, input)
	if err != nil {
		t.Fatal(err)
	}
	if !changed.Changed || changed.Route.ID != route || changed.Route.Name != name || (changed.Route.PathPrefix == nil || *changed.Route.PathPrefix != path) || changed.Route.Generation != before.Generation+1 || changed.Route.ETag == before.ETag || changed.Operation.State != "running" {
		t.Fatal("PATCH did not return changed canonical route plus running application operation")
	}
	stored, err := service.GetRoute(ctx, request, tunnel, route)
	if err != nil || stored.Name != name || (stored.PathPrefix == nil || *stored.PathPrefix != path) || stored.ETag != changed.Route.ETag {
		t.Fatalf("GET did not return persisted canonical mutation: %v", err)
	}
	replay, err := service.PatchRoute(ctx, request, tunnel, route, input)
	if err != nil || !replay.Replayed || replay.Route.Generation != stored.Generation || replay.Route.Name != name {
		t.Fatalf("lost response replay changed result: %v", err)
	}
	input.Mutation.IdempotencyKey += "-stale"
	if _, err := service.PatchRoute(ctx, request, tunnel, route, input); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale route ETag accepted: %v", err)
	}
}

func TestPublicHTTPCatchAllRouteIsRejectedOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account, host := f.addAccount(t, "catch-all")
	tunnel := "tun_catch_all_" + f.suffix
	created := f.createTunnel(t, account, host, tunnel, "catch-all-"+f.suffix)
	var route string
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT id FROM paperboat.tunnel_routes WHERE tunnel_id=$1 AND deleted_at IS NULL ORDER BY id LIMIT 1`, tunnel).Scan(&route); err != nil {
		t.Fatal(err)
	}
	service, err := NewResourceService(f.repository, ResourceConfig{CursorKey: bytes.Repeat([]byte("p"), 32), Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(false)
	request.Actor.AccountID, request.Actor.ActorID = account, account
	before, err := service.GetRoute(ctx, request, tunnel, route)
	if err != nil {
		t.Fatal(err)
	}
	catchAll, none := "catch_all", ""
	_, err = service.PatchRoute(ctx, request, tunnel, route, RoutePatchRequest{MatchType: &catchAll, Hostname: &none, WildcardSuffix: &none, Mutation: ResourceMutationInput{ExpectedGeneration: before.Generation, IdempotencyKey: "catch-all-public:" + f.suffix, RequestHash: sha256.Sum256([]byte("catch-all-public:" + f.suffix))}})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("public tunnel accepted a catch-all HTTP route: %v", err)
	}
	// A catch-all route that exists on a private tunnel must block publication.
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnels SET access_mode='private' WHERE id=$1`, tunnel); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_routes SET match_type='catch_all', match_hostname=NULL WHERE id=$1`, route); err != nil {
		t.Fatal(err)
	}
	public := AccessPublic
	_, err = f.repository.Patch(ctx, PatchRecord{
		OperationID: "op_catch_all_public_" + f.suffix, AuditEventID: "aud_catch_all_public_" + f.suffix,
		TunnelID: tunnel, AccountID: account, AccessMode: &public,
		ExpectedGeneration: created.Tunnel.Generation, IdempotencyKey: "catch-all-publish:" + f.suffix,
		RequestHash: sha256.Sum256([]byte("catch-all-publish:" + f.suffix)), ActorID: account, AuditActorID: host, ActorType: "host",
		RequestID: "req_catch_all_public_" + f.suffix, CorrelationID: "corr_catch_all_public_" + f.suffix,
		SourceMachineID: host, Now: f.now,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("private catch-all HTTP route became public: %v", err)
	}
}
