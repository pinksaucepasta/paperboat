package apitokens

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

const tokenColumns = `t.id, t.account_id, coalesce(t.team_id,''), t.name, 'user:'||u.primary_email,
 t.created_at, t.expires_at, t.last_used_at, t.revoked_at, t.scope`

// Create issues a token for actor in ws and returns it with its secret, which is only stored as
// a hash. With an idempotency key, a repeated create (same key, same request) within
// api_tokens.retry_seconds returns the same token and secret instead of a new token: the secret is
// held encrypted for that window only, so a response lost on the way back is not a lost secret.
func (s *Service) Create(ctx context.Context, actor string, ws workspace.Scope, in CreateInput, idempotencyKey string) (Token, string, error) {
	if !policy.ValidName(in.Name) {
		return Token{}, "", ErrInvalidName
	}
	if idempotencyKey != "" && !idempotencyKeyPattern.MatchString(idempotencyKey) {
		return Token{}, "", ErrInvalidIdempotencyKey
	}
	requestHash := createRequestHash(ws, in)
	lifetime, err := s.lifetime(in.ExpiresInDays)
	if err != nil {
		return Token{}, "", err
	}
	scope, errs := ParseScope(in.Scope)
	if len(errs) > 0 {
		return Token{}, "", &ScopeError{Errors: errs}
	}
	var scopeJSON []byte
	if scope != nil {
		if scopeJSON, err = json.Marshal(scope); err != nil {
			return Token{}, "", fmt.Errorf("encode api token scope: %w", err)
		}
	}
	id, err := newID()
	if err != nil {
		return Token{}, "", fmt.Errorf("create api token id: %w", err)
	}
	secret, hash, err := newSecret()
	if err != nil {
		return Token{}, "", fmt.Errorf("create api token secret: %w", err)
	}
	var token Token
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := workspace.ValidateTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		var scopeRows []policy.Row
		if scope != nil {
			w := teams.Workspace{TeamID: ws.TeamID}
			if ws.TeamID == "" {
				w.Owner = actor
			}
			rows, errs, err := teams.TokenScopeRowsTx(ctx, tx, w, scope)
			if err != nil {
				return err
			}
			if len(errs) > 0 {
				return &ScopeError{Errors: errs}
			}
			scopeRows = rows
		}
		// The user row lock serializes the per-member count with concurrent creates.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, actor); err != nil {
			return err
		}
		if idempotencyKey != "" {
			replayed, replaySecret, err := s.replayCreateTx(ctx, tx, actor, idempotencyKey, requestHash)
			if err != nil || replaySecret != "" {
				token, secret = replayed, replaySecret
				return err
			}
		}
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE account_id=$1 AND coalesce(team_id,'')=$2
		 AND revoked_at IS NULL AND expires_at>now()`, actor, ws.TeamID).Scan(&live); err != nil {
			return err
		}
		if live >= s.limits.MaxPerMember {
			return ErrLimit
		}
		now := s.now().UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO api_tokens(id, account_id, team_id, name, secret_hash, scope, created_at, expires_at)
		 VALUES($1,$2,nullif($3,''),$4,$5,$6,$7,$8)`, id, actor, ws.TeamID, in.Name, hash, scopeJSON, now, now.Add(lifetime)); err != nil {
			return mapInsertError(err)
		}
		for _, r := range scopeRows {
			for _, p := range r.Ports {
				if _, err := tx.Exec(ctx, `INSERT INTO api_token_scope_rules(token_id,entry,target_kind,target_id,action,ports) VALUES($1,$2,$3,$4,$5,int4range($6,$7,'[]'))`,
					id, r.Rule, r.TargetKind, r.TargetID, r.Action, p.From, p.To); err != nil {
					return err
				}
			}
		}
		if token, err = getTx(ctx, tx, id, false); err != nil {
			return err
		}
		if idempotencyKey != "" {
			if err := s.rememberCreateTx(ctx, tx, actor, idempotencyKey, requestHash, id, secret); err != nil {
				return err
			}
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorUser, EventType: "api_token.created",
			ResourceType: "api_token", ResourceID: id, Metadata: map[string]any{"workspace": ws.ID, "name": in.Name,
				"expires_at": token.ExpiresAt, "scoped": scope != nil}})
	})
	if err != nil {
		return Token{}, "", fmt.Errorf("create api token: %w", err)
	}
	return token, secret, nil
}

func mapInsertError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "api_tokens_live_name" {
		return ErrNameTaken
	}
	return err
}

// List returns actor's own tokens in ws, or every member's when actor owns or administers ws.
func (s *Service) List(ctx context.Context, actor string, ws workspace.Scope) ([]Token, error) {
	var tokens []Token
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := workspace.ValidateTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		admin, err := manages(ctx, tx, actor, ws)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+tokenColumns+` FROM api_tokens t JOIN users u ON u.id=t.account_id
		 WHERE coalesce(t.team_id,'')=$1 AND ($2 OR t.account_id=$3) ORDER BY t.created_at DESC, t.id`, ws.TeamID, admin, actor)
		if err != nil {
			return err
		}
		tokens, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Token, error) { return scanToken(row) })
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	return tokens, nil
}

// Revoke revokes token id in ws when actor created it or owns or administers ws. Revoking a
// revoked token returns it unchanged. A token actor may not see is ErrNotFound.
func (s *Service) Revoke(ctx context.Context, actor string, ws workspace.Scope, id string) (Token, error) {
	var token Token
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := workspace.ValidateTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		var err error
		if token, err = getTx(ctx, tx, id, true); err != nil {
			return err
		}
		if token.Workspace != ws {
			return ErrNotFound
		}
		if token.AccountID != actor {
			admin, err := manages(ctx, tx, actor, ws)
			if err != nil {
				return err
			}
			if !admin {
				return ErrNotFound
			}
		}
		if token.RevokedAt != nil {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at=$2, revoked_by=$3 WHERE id=$1`, id, s.now().UTC(), actor); err != nil {
			return err
		}
		// Its pb sessions end with it.
		if _, err := tx.Exec(ctx, `UPDATE cli_client_sessions SET state='revoked',revoked_at=$2,revocation_reason='api_token_revoked',version=version+1 WHERE api_token_id=$1 AND state='active'`, id, s.now().UTC()); err != nil {
			return err
		}
		if token, err = getTx(ctx, tx, id, false); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorUser, EventType: "api_token.revoked",
			ResourceType: "api_token", ResourceID: id, Metadata: map[string]any{"workspace": ws.ID, "name": token.Name, "created_by": token.AccountID}})
	})
	if err != nil {
		return Token{}, fmt.Errorf("revoke api token: %w", err)
	}
	return token, nil
}

// Authenticate maps a secret to its live token. Any secret that is not a live token of an
// active account is ErrInvalid. The secret is matched by its sha256 in the unique index, so
// no secret bytes are compared in Go. Workspace membership is checked per request by the
// workspace selection, which the caller pins to Principal.Workspace.
func (s *Service) Authenticate(ctx context.Context, secret string) (Principal, error) {
	if !wellFormed(secret) {
		return Principal{}, ErrInvalid
	}
	var p Principal
	var scopeJSON []byte
	var lastUsed sql.NullTime
	err := s.db.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		err := tx.QueryRow(ctx, `SELECT t.id, coalesce(t.team_id,''), t.scope, t.last_used_at,
		 u.id, u.workos_subject, u.primary_email, u.email_verified, u.display_name, u.status, u.role, u.created_at
		 FROM api_tokens t JOIN users u ON u.id=t.account_id
		 WHERE t.secret_hash=$1 AND t.revoked_at IS NULL AND t.expires_at>now() AND u.status='active'`, hashSecret(secret)).Scan(
			&p.TokenID, &p.Workspace.TeamID, &scopeJSON, &lastUsed, &p.User.ID, &p.User.WorkOSSubject, &p.User.PrimaryEmail,
			&p.User.EmailVerified, &p.User.DisplayName, &p.User.Status, (*string)(&p.User.Role), &p.User.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalid
		}
		if err != nil {
			return err
		}
		if now := s.now(); !lastUsed.Valid || now.Sub(lastUsed.Time) >= lastUsedInterval {
			_, err = tx.Exec(ctx, `UPDATE api_tokens SET last_used_at=$2 WHERE id=$1 AND (last_used_at IS NULL OR last_used_at<=$3)`,
				p.TokenID, now.UTC(), now.Add(-lastUsedInterval).UTC())
		}
		return err
	})
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			return Principal{}, ErrInvalid
		}
		return Principal{}, fmt.Errorf("authenticate api token: %w", err)
	}
	p.Workspace = scopeFor(p.Workspace.TeamID)
	if p.Scope, err = storedScope(scopeJSON); err != nil {
		return Principal{}, err
	}
	return p, nil
}

func getTx(ctx context.Context, tx *db.Tx, id string, lock bool) (Token, error) {
	query := `SELECT ` + tokenColumns + ` FROM api_tokens t JOIN users u ON u.id=t.account_id WHERE t.id=$1`
	if lock {
		query += ` FOR UPDATE OF t`
	}
	rows, err := tx.Query(ctx, query, id)
	if err != nil {
		return Token{}, err
	}
	token, err := pgx.CollectExactlyOneRow(rows, func(row pgx.CollectableRow) (Token, error) { return scanToken(row) })
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	return token, err
}

func scanToken(row pgx.Row) (Token, error) {
	var t Token
	var teamID string
	var lastUsed, revoked sql.NullTime
	var scopeJSON []byte
	if err := row.Scan(&t.ID, &t.AccountID, &teamID, &t.Name, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &lastUsed, &revoked, &scopeJSON); err != nil {
		return Token{}, err
	}
	t.Workspace = scopeFor(teamID)
	if lastUsed.Valid {
		t.LastUsedAt = &lastUsed.Time
	}
	if revoked.Valid {
		t.RevokedAt = &revoked.Time
	}
	var err error
	t.Scope, err = storedScope(scopeJSON)
	return t, err
}

func storedScope(raw []byte) ([]policy.ScopeEntry, error) {
	scope, errs := ParseScope(raw)
	if len(errs) > 0 {
		return nil, fmt.Errorf("stored api token scope: %w", errs)
	}
	return scope, nil
}

func scopeFor(teamID string) workspace.Scope {
	if teamID == "" {
		return workspace.Scope{ID: "personal"}
	}
	return workspace.Scope{ID: teamID, TeamID: teamID}
}

// manages reports whether actor owns or administers team ws, and so sees every member's
// tokens. Personal tokens are only ever their creator's.
func manages(ctx context.Context, tx *db.Tx, actor string, ws workspace.Scope) (bool, error) {
	if ws.TeamID == "" {
		return false, nil
	}
	var admin bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams t JOIN team_members m USING(team_id)
	 WHERE t.team_id=$1 AND m.account_id=$2 AND m.active AND (m.role='admin' OR t.owner_account=$2))`, ws.TeamID, actor).Scan(&admin)
	return admin, err
}
