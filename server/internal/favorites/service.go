package favorites

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

const Max = 5

var ErrLimit = errors.New("favorite limit reached")
var ErrResourceNotFound = errors.New("favorite resource not found")
var ErrInvalid = errors.New("invalid favorite")

type Favorite struct {
	Kind       string    `json:"kind"`
	ResourceID string    `json:"resource_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type Service struct{ store *db.DB }

func NewService(store *db.DB) *Service { return &Service{store: store} }

func (s *Service) List(ctx context.Context, userID string) ([]Favorite, error) {
	if scope, scoped := workspace.FromContext(ctx); scoped {
		var items []Favorite
		err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := teams.Lock(ctx, tx); err != nil {
				return err
			}
			if err := workspace.ValidateTx(ctx, tx, userID, scope); err != nil {
				return err
			}
			rows, err := tx.Queries().ListUserFavorites(ctx, userID)
			if err != nil {
				return err
			}
			items = make([]Favorite, 0, len(rows))
			for _, row := range rows {
				visible, err := favoriteVisibleInWorkspace(ctx, tx.Queries(), userID, scope, row.Kind, row.ResourceID)
				if err != nil {
					return err
				}
				if visible {
					items = append(items, Favorite{Kind: row.Kind, ResourceID: row.ResourceID, CreatedAt: row.CreatedAt})
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("list favorites: %w", err)
		}
		return items, nil
	}
	rows, err := s.store.Queries().ListUserFavorites(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list favorites: %w", err)
	}
	items := make([]Favorite, 0, len(rows))
	for _, row := range rows {
		items = append(items, Favorite{Kind: row.Kind, ResourceID: row.ResourceID, CreatedAt: row.CreatedAt})
	}
	return items, nil
}

func (s *Service) Set(ctx context.Context, userID, kind, resourceID string, favorite bool) error {
	kind, resourceID = strings.TrimSpace(kind), strings.TrimSpace(resourceID)
	if kind != "machine" && kind != "session" && kind != "preview" {
		return ErrInvalid
	}
	if resourceID == "" {
		return ErrInvalid
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		deleteParams := dbsqlc.DeleteUserFavoriteParams{UserID: userID, Kind: kind, ResourceID: resourceID}
		scope, scoped := workspace.FromContext(ctx)
		if scoped {
			if err := teams.Lock(ctx, tx); err != nil {
				return err
			}
			if err := workspace.ValidateTx(ctx, tx, userID, scope); err != nil {
				return err
			}
			visible, err := favoriteVisibleInWorkspace(ctx, tx.Queries(), userID, scope, kind, resourceID)
			if err != nil {
				return err
			}
			if !visible {
				return ErrResourceNotFound
			}
			if !favorite {
				_, err := tx.Queries().DeleteUserFavorite(ctx, deleteParams)
				return err
			}
			if _, err := tx.Queries().LockUserForFavorites(ctx, userID); err != nil {
				return err
			}
			count, err := tx.Queries().CountUserFavorites(ctx, userID)
			if err != nil {
				return err
			}
			created, err := tx.Queries().CreateUserFavorite(ctx, dbsqlc.CreateUserFavoriteParams{UserID: userID, Kind: kind, ResourceID: resourceID})
			if err != nil || created == 0 {
				return err
			}
			if count >= Max {
				return ErrLimit
			}
			return nil
		}
		if !favorite {
			_, err := tx.Queries().DeleteUserFavorite(ctx, deleteParams)
			return err
		}
		owned, err := tx.Queries().UserOwnsFavoriteResource(ctx, dbsqlc.UserOwnsFavoriteResourceParams{Kind: kind, OwnerUserID: userID, ResourceID: resourceID})
		if err != nil {
			return err
		}
		if !owned {
			return ErrResourceNotFound
		}
		if _, err := tx.Queries().LockUserForFavorites(ctx, userID); err != nil {
			return err
		}
		count, err := tx.Queries().CountUserFavorites(ctx, userID)
		if err != nil {
			return err
		}
		created, err := tx.Queries().CreateUserFavorite(ctx, dbsqlc.CreateUserFavoriteParams{UserID: userID, Kind: kind, ResourceID: resourceID})
		if err != nil {
			return err
		}
		if created == 0 {
			return nil
		}
		if count >= Max {
			return ErrLimit
		}
		return nil
	})
}

func favoriteVisibleInWorkspace(ctx context.Context, q *dbsqlc.Queries, userID string, scope workspace.Scope, kind, resourceID string) (bool, error) {
	switch kind {
	case "machine":
		teamID := sql.NullString{}
		if scope.TeamID != "" {
			teamID = sql.NullString{String: scope.TeamID, Valid: true}
		}
		_, err := q.GetVisibleUserMachineInWorkspace(ctx, dbsqlc.GetVisibleUserMachineInWorkspaceParams{
			ID: resourceID, WorkspaceSet: true, UserID: userID, WorkspaceTeamID: teamID,
		})
		if favoriteRowAbsent(err) {
			return false, nil
		}
		return err == nil, err
	case "session":
		machineID, sessionID, ok := strings.Cut(resourceID, ":")
		if !ok || machineID == "" || sessionID == "" || strings.Contains(sessionID, ":") {
			return false, nil
		}
		teamID := sql.NullString{}
		if scope.TeamID != "" {
			teamID = sql.NullString{String: scope.TeamID, Valid: true}
		}
		_, err := q.GetUserMachineTerminalSessionInWorkspace(ctx, dbsqlc.GetUserMachineTerminalSessionInWorkspaceParams{
			ID: sessionID, UserMachineID: machineID, UserID: userID, WorkspaceTeamID: teamID,
		})
		if favoriteRowAbsent(err) {
			return false, nil
		}
		return err == nil, err
	default:
		// Preview favorites have no active producer; do not disclose stale rows
		// through a workspace-scoped collection.
		return false, nil
	}
}

// The generated workspace lookups use pgx.Row.Scan, which returns this exact
// sentinel for absence. A joined or wrapped I/O failure must not look like an
// invisible resource.
func favoriteRowAbsent(err error) bool { return err == pgx.ErrNoRows }
