package controlplane

import (
	"context"
	"database/sql"
	"regexp"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

var adminKeyID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type UsageKey struct {
	KeyID      string     `json:"key_id"`
	EdgeNodeID string     `json:"edge_node_id"`
	NotBefore  time.Time  `json:"not_before"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

type UsageKeyPage struct {
	Total     int64      `json:"total"`
	Items     []UsageKey `json:"items"`
	NextAfter string     `json:"next_after,omitempty"`
}

// ListUsageKeys pages the edge usage verification keys by key id, revoked ones included. Admin only, like the writes.
func (s *EdgeService) ListUsageKeys(ctx context.Context, actorID, after string, limit int) (UsageKeyPage, error) {
	out := UsageKeyPage{Items: []UsageKey{}}
	if actorID == "" || limit < 1 || limit > 100 || (after != "" && !adminKeyID.MatchString(after)) {
		return UsageKeyPage{}, invalidUsage("edge_admin_lists.go:33")
	}
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireActiveGlobalRole(ctx, tx, actorID, "admin"); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM control_usage_verification_keys`).Scan(&out.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT key_id,edge_node_id,not_before,expires_at,revoked_at,created_at FROM control_usage_verification_keys WHERE key_id>$1 ORDER BY key_id LIMIT $2`, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v UsageKey
			var revoked sql.NullTime
			if err := rows.Scan(&v.KeyID, &v.EdgeNodeID, &v.NotBefore, &v.ExpiresAt, &revoked, &v.CreatedAt); err != nil {
				return err
			}
			if len(out.Items) == limit {
				out.NextAfter = out.Items[len(out.Items)-1].KeyID
				break
			}
			if revoked.Valid {
				v.RevokedAt = &revoked.Time
			}
			out.Items = append(out.Items, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actorID, ActorType: audit.ActorAdmin, EventType: "edge.usage_keys_listed", ResourceType: "usage_key", ResourceID: "*"})
	})
	if err != nil {
		return UsageKeyPage{}, err
	}
	return out, nil
}

type SigningKey struct {
	mint.KeyState
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

type SigningKeyPage struct {
	Total int64        `json:"total"`
	Items []SigningKey `json:"items"`
}

// ListSigningKeys lists the mint's signing keys (from configuration) with their operator revocations.
// shortcut: one page; the set is bounded by the trust document's key cap (jwks.MaxKeys), page it if that cap grows.
func (s *EdgeService) ListSigningKeys(ctx context.Context, actorID string) (SigningKeyPage, error) {
	out := SigningKeyPage{Items: []SigningKey{}}
	if actorID == "" || s.signer == nil {
		return SigningKeyPage{}, invalidUsage("edge_admin_lists.go:87")
	}
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireActiveGlobalRole(ctx, tx, actorID, "admin"); err != nil {
			return err
		}
		revoked := map[string]SigningKey{}
		rows, err := tx.Query(ctx, `SELECT key_id,reason,revoked_at FROM control_signing_key_revocations`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v SigningKey
			var at time.Time
			if err := rows.Scan(&v.ID, &v.Reason, &at); err != nil {
				return err
			}
			v.RevokedAt = &at
			revoked[v.ID] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, k := range s.signer.KeyStates() {
			item := SigningKey{KeyState: k}
			if r, ok := revoked[k.ID]; ok {
				item.RevokedAt, item.Reason = r.RevokedAt, r.Reason
			}
			out.Items = append(out.Items, item)
		}
		out.Total = int64(len(out.Items))
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actorID, ActorType: audit.ActorAdmin, EventType: "mint.signing_keys_listed", ResourceType: "mint_signing_key", ResourceID: "*"})
	})
	if err != nil {
		return SigningKeyPage{}, err
	}
	return out, nil
}
