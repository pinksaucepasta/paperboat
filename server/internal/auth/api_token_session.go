package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

var (
	// ErrAPITokenUnusable: the API token is revoked, expired or not its creator's.
	ErrAPITokenUnusable = errors.New("api token is revoked or expired")
	// ErrOutsideTokenScope: the request's API token scope does not cover the machine action.
	ErrOutsideTokenScope = errors.New("this API token's scope does not cover this machine action")
)

// APITokenSessionRequest describes the computer that signs in with an API token.
type APITokenSessionRequest struct {
	TokenID, Account, Label, DeviceType, OS string
}

// StartAPITokenSession turns a live API token into an ordinary CLI session of its creator that
// carries the token (PLAN F18-06): pb works as after a browser login, the session ends with the
// token, and every machine authorization of it also checks the token's scope.
func (s *DeviceService) StartAPITokenSession(ctx context.Context, in APITokenSessionRequest) (TokenSet, error) {
	if in.Label == "" || len(in.Label) > s.cfg.MaxClientLabelLength || strings.TrimSpace(in.OS) == "" || !slices.Contains([]string{"desktop", "server", "container"}, in.DeviceType) {
		return TokenSet{}, &DeviceError{Code: "validation_failed"}
	}
	access, refresh := randomToken(32), randomToken(32)
	sessionID := newID("session")
	scopes := normalizedScopes(s.cfg.AllowedScopes)
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var expires time.Time
		err := tx.QueryRow(ctx, `SELECT expires_at FROM api_tokens WHERE id=$1 AND account_id=$2 AND revoked_at IS NULL AND expires_at>now() FOR UPDATE`, in.TokenID, in.Account).Scan(&expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAPITokenUnusable
		}
		if err != nil {
			return err
		}
		now := s.now()
		session := dbsqlc.CreateClientSessionParams{ID: sessionID, UserID: in.Account, ClientID: s.cfg.ClientID, ClientLabel: in.Label, DeviceType: in.DeviceType, Os: strings.TrimSpace(in.OS), Scopes: scopes, CreatedAt: now, ApprovedAt: now}
		if err := CreateClientSessionTx(ctx, tx, session, access, refresh, s.tokenPolicy()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cli_client_sessions SET api_token_id=$2 WHERE id=$1`, sessionID, in.TokenID); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: in.Account, ActorType: audit.ActorUser, EventType: "auth.client.api_token", ResourceType: "client_session", ResourceID: sessionID,
			IdempotencyKey: "auth.client.api_token:" + sessionID, Metadata: map[string]any{"api_token_id": in.TokenID, "token_expires_at": expires}})
	})
	if err != nil {
		return TokenSet{}, err
	}
	return TokenSet{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", ExpiresIn: int(s.cfg.AccessTokenLifetime / time.Second), Scope: scopes, CLIClientSessionID: sessionID}, nil
}

type apiTokenKey struct{}

// WithAPIToken marks ctx as a request made with an API token (directly or through a token-backed
// session); machine authorizations then also check the token's scope.
func WithAPIToken(ctx context.Context, tokenID string) context.Context {
	return context.WithValue(ctx, apiTokenKey{}, tokenID)
}

// APITokenFromContext returns the API token the request was made with, if any.
func APITokenFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(apiTokenKey{}).(string)
	return id, ok && id != ""
}
