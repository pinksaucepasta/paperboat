package previewtunnelstore

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// BeginOperation records an idempotent preview or tunnel operation, or returns the earlier operation
// stored under the same account and key (replayed=true). Every preview, tunnel, route and domain
// mutation begins here, so the replay rule lives in one place: a replay must match the operation
// type, resource kind and request hash and, when matchResource is set, the resource ID. A mismatch
// is ErrIdempotencyConflict; database errors are returned as they are for the caller to map.
func BeginOperation(ctx context.Context, q *dbsqlc.Queries, params dbsqlc.CreatePreviewTunnelOperationParams, matchResource bool) (dbsqlc.Operation, bool, error) {
	operation, err := q.CreatePreviewTunnelOperation(ctx, params)
	if !errors.Is(err, pgx.ErrNoRows) {
		return operation, false, err
	}
	existing, err := q.GetPreviewTunnelOperationByIdempotency(ctx, dbsqlc.GetPreviewTunnelOperationByIdempotencyParams{
		AccountID: params.AccountID, IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		return dbsqlc.Operation{}, false, err
	}
	if existing.OperationType != params.OperationType || existing.ResourceKind != params.ResourceKind ||
		!bytes.Equal(existing.RequestHash, params.RequestHash) ||
		matchResource && (!existing.ResourceID.Valid || existing.ResourceID.String != params.ResourceID.String) {
		return dbsqlc.Operation{}, false, ErrIdempotencyConflict
	}
	return existing, true, nil
}
