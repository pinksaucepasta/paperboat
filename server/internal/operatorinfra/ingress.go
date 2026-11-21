package operatorinfra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// verifyNodeIngress probes a ready managed edge's public address with its own credential and records the verified
// addresses, fenced on the probed process epoch, endpoint, revision (0: the current one) and credential.
func (s *Service) verifyNodeIngress(ctx context.Context, tx *db.Tx, node string, revision int64) error {
	var host, epoch string
	var cipher, runtimeHash []byte
	err := tx.QueryRow(ctx, `SELECT n.endpoint_host,n.process_epoch,m.runtime_ciphertext,m.runtime_hash,m.revision FROM control_tunnel_nodes n JOIN managed_node_authority m ON m.node_id=n.id JOIN operator_regions r ON r.id=m.region_id WHERE n.id=$1 AND ($2=0 OR m.revision=$2) AND m.retired_at IS NULL AND r.enabled AND n.state='ready' AND n.ready AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now() AND 'edge'=ANY(n.roles)`, node, revision).Scan(&host, &epoch, &cipher, &runtimeHash, &revision)
	if err == pgx.ErrNoRows {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	credential, err := secrets.Decrypt(s.key, cipher)
	if err != nil {
		return err
	}
	check := sha256.Sum256([]byte(credential))
	if !bytes.Equal(check[:], runtimeHash) {
		return ErrConflict
	}
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	v4, v6, err := s.probeIngress(probeCtx, host, node, credential)
	if err != nil {
		return ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE control_tunnel_nodes n SET public_ingress_ipv4=$3::inet,public_ingress_ipv6=$4::inet,public_ingress_verified_at=now() WHERE n.id=$1 AND n.process_epoch=$2 AND n.endpoint_host=$5 AND n.state='ready' AND n.ready AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now() AND 'edge'=ANY(n.roles) AND EXISTS (SELECT 1 FROM managed_node_authority m JOIN operator_regions r ON r.id=m.region_id WHERE m.node_id=n.id AND m.revision=$6 AND m.runtime_hash=$7 AND m.retired_at IS NULL AND r.enabled)`, node, epoch, v4, v6, host, revision, runtimeHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// Ingress retry backoff: a node whose probe fails is tried again after ingressRetryFirst, doubling up to ingressRetryMax.
const (
	ingressRetryFirst = 15 * time.Second
	ingressRetryMax   = 10 * time.Minute
	ingressBatch      = 64
)

type ingressRetry struct {
	failures int
	next     time.Time
}

// ReconcileIngress verifies, without an operator, every ready managed edge whose ingress is not verified: a new node,
// or one whose endpoint changed (update clears the verification). Failed probes back off per node. Run by the worker
// leader only. The verify-ingress action stays as a manual re-check.
// Prior art: the self-hosted equivalent, selfhost.Service.ReconcileIngress.
func (s *Service) ReconcileIngress(ctx context.Context) error {
	rows, err := s.store.Pool().Query(ctx, `SELECT n.id FROM paperboat.control_tunnel_nodes n
JOIN paperboat.managed_node_authority m ON m.node_id=n.id AND m.retired_at IS NULL
JOIN paperboat.operator_regions r ON r.id=m.region_id AND r.enabled
WHERE n.state='ready' AND n.ready AND n.public_ingress_verified_at IS NULL AND 'edge'=ANY(n.roles)
 AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now() ORDER BY n.id LIMIT $1`, ingressBatch)
	if err != nil {
		return err
	}
	nodes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	now := time.Now()
	s.ingressMu.Lock()
	retries := make(map[string]ingressRetry, len(nodes)) // only nodes still waiting keep a backoff
	for _, node := range nodes {
		if r, ok := s.ingressRetries[node]; ok {
			retries[node] = r
		}
	}
	s.ingressRetries = retries
	s.ingressMu.Unlock()
	for _, node := range nodes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.ingressMu.Lock()
		r := s.ingressRetries[node]
		s.ingressMu.Unlock()
		if now.Before(r.next) {
			continue
		}
		err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error { return s.verifyNodeIngress(ctx, tx, node, 0) })
		s.ingressMu.Lock()
		if err == nil {
			delete(s.ingressRetries, node)
		} else {
			r.failures++
			r.next = now.Add(min(ingressRetryFirst<<min(r.failures-1, 10), ingressRetryMax))
			s.ingressRetries[node] = r
			slog.WarnContext(ctx, "managed_ingress_verify_failed", "node_id", node, "failures", r.failures, "retry_at", r.next, "error", err)
		}
		s.ingressMu.Unlock()
	}
	return nil
}
