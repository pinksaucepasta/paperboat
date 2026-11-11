package runtimeattachment

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// withinAllowance is the quota checker of a workspace that has bandwidth left: the accounting authority admits
// against a checker or fails closed, as production wires the billing service.
type withinAllowance struct{}

func (withinAllowance) CheckWorkspaceBandwidthTx(context.Context, *db.Tx, string, time.Time) (bool, error) {
	return false, nil
}
