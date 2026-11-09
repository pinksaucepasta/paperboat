// Package accountprefs owns an account's preferences: today its default file transfer mode.
package accountprefs

import (
	"context"
	"database/sql"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// File transfer modes (PLAN 3b): Automatic is bounded and may use Paperboat-hosted relays;
// Unlimited is unbounded and only uses direct paths and self-hosted relays.
const (
	TransferModeAutomatic = "automatic"
	TransferModeUnlimited = "unlimited"
)

var (
	ErrInvalid  = errors.New("preference value is invalid")
	ErrNotFound = errors.New("account not found")
)

type Preferences struct {
	FileTransferMode string `json:"file_transfer_mode"`
	// ENVNamesListable: commands on the account's machines may list the names (never values) of its personal ENV.
	ENVNamesListable bool `json:"env_names_listable"`
}

// Update is a partial change; a nil field keeps its value.
type Update struct {
	FileTransferMode *string `json:"file_transfer_mode"`
	ENVNamesListable *bool   `json:"env_names_listable"`
}

type Service struct{ db *db.DB }

func New(store *db.DB) *Service { return &Service{db: store} }

func (s *Service) Get(ctx context.Context, accountID string) (Preferences, error) {
	var out Preferences
	err := s.db.SQL().QueryRowContext(ctx, `SELECT file_transfer_mode,env_names_listable FROM paperboat.users WHERE id=$1`, accountID).Scan(&out.FileTransferMode, &out.ENVNamesListable)
	if errors.Is(err, sql.ErrNoRows) {
		return Preferences{}, ErrNotFound
	}
	return out, err
}

func (s *Service) Set(ctx context.Context, accountID string, update Update) (Preferences, error) {
	if mode := update.FileTransferMode; mode != nil && *mode != TransferModeAutomatic && *mode != TransferModeUnlimited {
		return Preferences{}, ErrInvalid
	}
	if update.FileTransferMode != nil || update.ENVNamesListable != nil {
		result, err := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.users SET file_transfer_mode=coalesce($2,file_transfer_mode),env_names_listable=coalesce($3,env_names_listable) WHERE id=$1`, accountID, update.FileTransferMode, update.ENVNamesListable)
		if err != nil {
			return Preferences{}, err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return Preferences{}, err
			}
			return Preferences{}, ErrNotFound
		}
	}
	return s.Get(ctx, accountID)
}
