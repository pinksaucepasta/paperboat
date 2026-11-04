package tunnelcert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// DistributionAction is one pending or completed edge certificate action. Message never carries key material;
// a stage action keeps its certificate and key sealed with the certificate master key.
type DistributionAction struct {
	Key                string
	Message            DistributionMessage
	MasterKeyReference string
	SealedCertificate  []byte
	SealedPrivateKey   []byte
	Status             string
	Code               string
}

// DistributionActionStore holds the hub's actions where every server instance sees them (F21-52).
type DistributionActionStore interface {
	// Put inserts the action, or replaces a failed or expired one with the same key, and returns the stored
	// action (an existing live or completed one wins: retries are idempotent).
	Put(context.Context, DistributionAction) (DistributionAction, error)
	Get(context.Context, string) (DistributionAction, bool, error)
	// Pending lists unexpired, unacknowledged actions of one edge process after a key, in key order.
	Pending(ctx context.Context, nodeID, epoch, after string, limit int, now time.Time) ([]DistributionAction, error)
	// Complete records an edge result for a pending, unexpired action; false when there is none.
	Complete(ctx context.Context, key, status, code string, now time.Time) (bool, error)
	CountPending(context.Context, time.Time) (int, error)
	// Purge deletes expired pending actions and results older than the retention.
	Purge(context.Context, time.Time) error
}

// distributionResultRetention bounds the result ledger; a coordinator retry rebuilds anything older.
const distributionResultRetention = time.Hour

func distributionStoreKey(messageKey string) string {
	sum := sha256.Sum256([]byte(messageKey))
	return hex.EncodeToString(sum[:])
}

type sqlDistributionActionStore struct{ db *db.DB }

func NewSQLDistributionActionStore(database *db.DB) (DistributionActionStore, error) {
	if database == nil || database.Pool() == nil {
		return nil, fmt.Errorf("%w: database is required", ErrDistributionTransportInvalid)
	}
	return sqlDistributionActionStore{db: database}, nil
}

const distributionActionColumns = `action_key, message, master_key_reference, sealed_certificate, sealed_private_key, coalesce(status,''), coalesce(code,'')`

func scanDistributionAction(row pgx.Row) (DistributionAction, error) {
	var a DistributionAction
	var message []byte
	if err := row.Scan(&a.Key, &message, &a.MasterKeyReference, &a.SealedCertificate, &a.SealedPrivateKey, &a.Status, &a.Code); err != nil {
		return DistributionAction{}, err
	}
	if err := json.Unmarshal(message, &a.Message); err != nil {
		return DistributionAction{}, fmt.Errorf("%w: stored distribution action: %w", ErrDistributionTransportInvalid, err)
	}
	return a, nil
}

func (s sqlDistributionActionStore) Put(ctx context.Context, a DistributionAction) (DistributionAction, error) {
	message, err := json.Marshal(a.Message)
	if err != nil {
		return DistributionAction{}, err
	}
	if _, err = s.db.Pool().Exec(ctx, `INSERT INTO paperboat.tunnel_certificate_distribution_actions
 (action_key, edge_node_id, edge_process_epoch, message, master_key_reference, sealed_certificate, sealed_private_key, expires_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
 ON CONFLICT (action_key) DO UPDATE SET message=excluded.message, master_key_reference=excluded.master_key_reference,
   sealed_certificate=excluded.sealed_certificate, sealed_private_key=excluded.sealed_private_key,
   expires_at=excluded.expires_at, status=NULL, code=NULL, completed_at=NULL
 WHERE tunnel_certificate_distribution_actions.status='failed'
   OR (tunnel_certificate_distribution_actions.status IS NULL AND tunnel_certificate_distribution_actions.expires_at <= now())`,
		a.Key, a.Message.EdgeNodeID, a.Message.EdgeProcessEpoch, message, a.MasterKeyReference, a.SealedCertificate, a.SealedPrivateKey, a.Message.ExpiresAt); err != nil {
		return DistributionAction{}, err
	}
	stored, found, err := s.Get(ctx, a.Key)
	if err == nil && !found {
		err = fmt.Errorf("%w: distribution action vanished after insert", ErrDistributionTransportFailed)
	}
	return stored, err
}

func (s sqlDistributionActionStore) Get(ctx context.Context, key string) (DistributionAction, bool, error) {
	a, err := scanDistributionAction(s.db.Pool().QueryRow(ctx, `SELECT `+distributionActionColumns+` FROM paperboat.tunnel_certificate_distribution_actions WHERE action_key=$1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return DistributionAction{}, false, nil
	}
	return a, err == nil, err
}

func (s sqlDistributionActionStore) Pending(ctx context.Context, nodeID, epoch, after string, limit int, now time.Time) ([]DistributionAction, error) {
	rows, err := s.db.Pool().Query(ctx, `SELECT `+distributionActionColumns+` FROM paperboat.tunnel_certificate_distribution_actions
 WHERE edge_node_id=$1 AND edge_process_epoch=$2 AND status IS NULL AND action_key > $3 AND expires_at > $4
 ORDER BY action_key LIMIT $5`, nodeID, epoch, after, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DistributionAction
	for rows.Next() {
		a, err := scanDistributionAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s sqlDistributionActionStore) Complete(ctx context.Context, key, status, code string, now time.Time) (bool, error) {
	tag, err := s.db.Pool().Exec(ctx, `UPDATE paperboat.tunnel_certificate_distribution_actions
 SET status=$2, code=nullif($3,''), completed_at=$4, sealed_certificate=NULL, sealed_private_key=NULL
 WHERE action_key=$1 AND status IS NULL AND expires_at > $4`, key, status, code, now)
	return tag.RowsAffected() == 1, err
}

func (s sqlDistributionActionStore) CountPending(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := s.db.Pool().QueryRow(ctx, `SELECT count(*) FROM paperboat.tunnel_certificate_distribution_actions WHERE status IS NULL AND expires_at > $1`, now).Scan(&n)
	return n, err
}

func (s sqlDistributionActionStore) Purge(ctx context.Context, now time.Time) error {
	_, err := s.db.Pool().Exec(ctx, `DELETE FROM paperboat.tunnel_certificate_distribution_actions
 WHERE (status IS NULL AND expires_at <= $1) OR completed_at <= $2`, now, now.Add(-distributionResultRetention))
	return err
}

// MemoryDistributionActionStore keeps actions in one process.
// shortcut: single process and no result retention, for tests only; production wires NewSQLDistributionActionStore.
type MemoryDistributionActionStore struct {
	mu      sync.Mutex
	actions map[string]DistributionAction
}

func NewMemoryDistributionActionStore() *MemoryDistributionActionStore {
	return &MemoryDistributionActionStore{actions: map[string]DistributionAction{}}
}

func (m *MemoryDistributionActionStore) Put(_ context.Context, a DistributionAction) (DistributionAction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.actions[a.Key]; ok && existing.Status != "failed" && (existing.Status != "" || existing.Message.ExpiresAt.After(time.Now())) {
		return existing, nil
	}
	m.actions[a.Key] = a
	return a, nil
}

func (m *MemoryDistributionActionStore) Get(_ context.Context, key string) (DistributionAction, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.actions[key]
	return a, ok, nil
}

func (m *MemoryDistributionActionStore) Pending(_ context.Context, nodeID, epoch, after string, limit int, now time.Time) ([]DistributionAction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DistributionAction
	for key, a := range m.actions {
		if a.Status == "" && a.Message.EdgeNodeID == nodeID && a.Message.EdgeProcessEpoch == epoch && key > after && a.Message.ExpiresAt.After(now) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryDistributionActionStore) Complete(_ context.Context, key, status, code string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.actions[key]
	if !ok || a.Status != "" || !a.Message.ExpiresAt.After(now) {
		return false, nil
	}
	a.Status, a.Code, a.SealedCertificate, a.SealedPrivateKey = status, code, nil, nil
	m.actions[key] = a
	return true, nil
}

func (m *MemoryDistributionActionStore) CountPending(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.actions {
		if a.Status == "" && a.Message.ExpiresAt.After(now) {
			n++
		}
	}
	return n, nil
}

func (m *MemoryDistributionActionStore) Purge(_ context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, a := range m.actions {
		if a.Status == "" && !a.Message.ExpiresAt.After(now) {
			delete(m.actions, key)
		}
	}
	return nil
}
