package runtimeattachment

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// openQuota admits every request: these fixtures check what the edge is told to serve, not how much bandwidth
// is left (the projection fails closed without a quota checker).
type openQuota struct{}

func (openQuota) CheckWorkspaceBandwidthTx(context.Context, *db.Tx, string, time.Time) (bool, error) {
	return false, nil // not exhausted
}
