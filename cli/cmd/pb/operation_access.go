package main

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/cli/internal/auth"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

// Standing-rule operations have no descriptor to release. For the others, keep
// cleanup separate from the command's result and bound it even after cancellation.
func releaseOperationCredentials(ctx context.Context, d *deps, machineID, operationID, capability string) {
	source, ok := d.auth.(*sessionauth.Source)
	if !ok {
		return
	}
	perOperation, err := source.ServerAuthorized()
	if err != nil || !perOperation {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	client := api.New(d.cfg.ServerURL, config.Credential{}, nil)
	client.SetCredentialSource(source.WithContext(ctx))
	_ = client.ReleaseOperationAccess(ctx, machineID, operationID, capability)
}
