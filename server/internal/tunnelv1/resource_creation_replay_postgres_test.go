package tunnelv1

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
)

func TestDomainCreateLostResponseReplayOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account, host := f.addAccount(t, "domain-replay")
	tunnelID := "tun_replay_" + f.suffix
	f.createTunnel(t, account, host, tunnelID, "domain-replay")
	var routeID string
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT id FROM paperboat.tunnel_routes WHERE tunnel_id=$1 AND deleted_at IS NULL ORDER BY id LIMIT 1`, tunnelID).Scan(&routeID); err != nil {
		t.Fatal(err)
	}
	input := DomainRecord{OperationID: "op_domain_replay_" + f.suffix, AuditEventID: "aud_domain_replay_" + f.suffix, AccountID: account, TunnelID: tunnelID, DomainID: "dom_original_" + f.suffix, RouteID: routeID, Hostname: "replay-" + f.suffix + ".example.test", MatchType: "exact", ChallengeReference: "dns-challenge://" + f.suffix, DNSTarget: "target.example.test", IdempotencyKey: "domain-replay:" + f.suffix, RequestHash: sha256.Sum256([]byte("same-domain-body")), ActorID: account, AuditActorID: host, ActorType: "host", RequestID: "req_domain_replay_" + f.suffix, CorrelationID: "corr_domain_replay_" + f.suffix, SourceMachineID: host, Now: f.now}
	first, err := f.repository.CreateResourceDomain(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	// A caller that lost the first response generates fresh server-owned IDs on
	// retry. The original operation and resource remain authoritative.
	retry := input
	retry.OperationID = "op_retry_" + f.suffix
	retry.DomainID = "dom_retry_" + f.suffix
	retry.AuditEventID = "aud_retry_" + f.suffix
	retry.ChallengeReference = "dns-challenge://retry-" + f.suffix
	replay, err := f.repository.CreateResourceDomain(ctx, retry)
	if err != nil || !replay.Replayed || replay.Domain.ID != first.Domain.ID || replay.Operation.ID != first.Operation.ID {
		t.Fatalf("replay domain=%s operation=%s replayed=%v err=%v", replay.Domain.ID, replay.Operation.ID, replay.Replayed, err)
	}
	var count int
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.tunnel_domains WHERE tunnel_id=$1`, tunnelID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("domain count=%d err=%v", count, err)
	}
	retry.RequestHash = sha256.Sum256([]byte("changed-domain-body"))
	if _, err := f.repository.CreateResourceDomain(ctx, retry); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed body=%v", err)
	}
	retry = input
	retry.TunnelID = "tun_other_replay_" + f.suffix
	f.createTunnel(t, account, host, retry.TunnelID, "other-domain-replay")
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT id FROM paperboat.tunnel_routes WHERE tunnel_id=$1 AND deleted_at IS NULL ORDER BY id LIMIT 1`, retry.TunnelID).Scan(&retry.RouteID); err != nil {
		t.Fatal(err)
	}
	// Even a repository caller supplying the old body hash cannot move the
	// stored result to another parent: the replay lookup remains parent scoped.
	if _, err := f.repository.CreateResourceDomain(ctx, retry); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("changed parent=%v", err)
	}
}
