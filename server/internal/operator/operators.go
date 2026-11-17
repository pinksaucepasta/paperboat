package operator

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Operator management reads (admin only). They never return password hashes, TOTP secrets, recovery codes or
// invitation tokens; a session id is the hex SHA-256 of its token, which cannot be turned back into the token.

type OperatorSummary struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	MFAEnrolled bool      `json:"mfa_enrolled"`
	Invited     bool      `json:"invited"`
	Sessions    int64     `json:"live_sessions"`
	CreatedAt   time.Time `json:"created_at"`
}
type OperatorPage struct {
	Counted
	Operators []OperatorSummary `json:"operators"`
	NextAfter string            `json:"next_after,omitempty"`
}
type OperatorSession struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	MFAPending bool      `json:"mfa_pending"`
}
type OperatorSessions struct {
	OperatorID string            `json:"operator_id"`
	Email      string            `json:"email"`
	Sessions   []OperatorSession `json:"sessions"`
}

func readOperators(ctx context.Context, tx *db.Tx, after string, limit int) (_ OperatorPage, resultErr error) {
	p := OperatorPage{Operators: make([]OperatorSummary, 0, limit)}
	var err error
	if p.Counted, err = countRows(ctx, tx, ` paperboat.operator_accounts WHERE true`); err != nil {
		return p, err
	}
	rows, err := tx.Query(ctx, `SELECT a.id,a.email,a.role,a.status,
 EXISTS(SELECT 1 FROM paperboat.operator_totp t WHERE t.operator_id=a.id AND t.confirmed_at IS NOT NULL),
 EXISTS(SELECT 1 FROM paperboat.operator_invitations i WHERE i.operator_id=a.id AND i.expires_at>now()),
 (SELECT count(*) FROM paperboat.operator_sessions s WHERE s.operator_id=a.id AND s.revoked_at IS NULL AND s.expires_at>now()),
 a.created_at FROM paperboat.operator_accounts a WHERE a.id>$1 ORDER BY a.id LIMIT $2`, after, limit+1)
	if err != nil {
		return p, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	for rows.Next() {
		var v OperatorSummary
		if err := rows.Scan(&v.ID, &v.Email, &v.Role, &v.Status, &v.MFAEnrolled, &v.Invited, &v.Sessions, &v.CreatedAt); err != nil {
			return p, err
		}
		if len(p.Operators) == limit {
			p.NextAfter = p.Operators[len(p.Operators)-1].ID
			break
		}
		p.Operators = append(p.Operators, v)
	}
	return p, rows.Err()
}

// readOperatorSessions lists the operator's live sessions (at most 100; an operator has a handful).
func readOperatorSessions(ctx context.Context, tx *db.Tx, id string) (_ OperatorSessions, resultErr error) {
	out := OperatorSessions{OperatorID: id, Sessions: []OperatorSession{}}
	if err := tx.QueryRow(ctx, `SELECT email FROM paperboat.operator_accounts WHERE id=$1`, id).Scan(&out.Email); err != nil {
		return out, scanErr(err)
	}
	rows, err := tx.Query(ctx, `SELECT token_hash,created_at,expires_at,mfa_pending FROM paperboat.operator_sessions WHERE operator_id=$1 AND revoked_at IS NULL AND expires_at>now() ORDER BY created_at DESC LIMIT 100`, id)
	if err != nil {
		return out, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	for rows.Next() {
		var token []byte
		var v OperatorSession
		if err := rows.Scan(&token, &v.CreatedAt, &v.ExpiresAt, &v.MFAPending); err != nil {
			return out, err
		}
		v.ID = hex.EncodeToString(token)
		out.Sessions = append(out.Sessions, v)
	}
	return out, rows.Err()
}
