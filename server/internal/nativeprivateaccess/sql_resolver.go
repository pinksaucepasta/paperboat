package nativeprivateaccess

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

type SQLResolver struct {
	AdmissionLifetime time.Duration
	database          *sql.DB
	now               func() time.Time
}

func NewSQLResolver(database *db.DB) (*SQLResolver, error) {
	if database == nil || database.SQL() == nil {
		return nil, ErrInvalid
	}
	return &SQLResolver{database: database.SQL(), now: func() time.Time { return time.Now().UTC() }}, nil
}

// ResolveNativePrivate authorizes an explicit machine service independently
// of terminal sessions, using current owner/team policy and runtime state.
func (r *SQLResolver) ResolveNativePrivate(ctx context.Context, in Request) (Target, error) {
	if r == nil || r.database == nil || ctx == nil || in.AccountID == "" || in.UserID != in.AccountID || in.CLIClientSessionID == "" || in.ResourceKind != "machine_service" || in.Protocol != "tcp" {
		return Target{}, ErrInvalid
	}
	return r.resolveMachineService(ctx, in)
}

func mapResolveError(err error) error {
	if err == nil {
		return nil
	}
	if pureNativeRejection(err, sql.ErrNoRows) {
		return ErrDenied
	}
	return fmt.Errorf("resolve native private target: %w", err)
}
