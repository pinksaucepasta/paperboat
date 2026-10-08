package tunnelenrollment

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func openObservedProductionAssembly(ctx context.Context, config tunnelmanager.ProductionAssemblyConfig) (*tunnelmanager.ProductionAssembly, error) {
	assembly, status, err := tunnelmanager.OpenProductionAssembly(config)
	if err != nil || !status.Degraded {
		return assembly, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if recorder := diagnostics.FromContext(ctx); recorder != nil {
		fields := map[string]string{"operation": "tunnel_bootstrap", "outcome": "success", "reason": startupRecoveryReason(status), "state": startupRecoverySource(status)}
		if recordErr := recorder.RecordWithSupportReference("runtime", "recovered", "warning", supportref.FromContext(ctx), fields); recordErr != nil {
			errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "tunnel_bootstrap", "diagnostic_storage", "diagnostic_storage_unavailable", recordErr)
		}
	}
	errorreport.Current().Lifecycle(ctx, "config", "tunnel_bootstrap", "recovered", "success")
	return assembly, nil
}

func startupRecoveryReason(status hoststate.StartupStatus) string {
	switch status.Code {
	case "backup_corrupt_before_migration", "backup_corrupt_repaired", "backup_legacy_repaired", "backup_stale_repaired", "backup_missing_repaired", "primary_recovered_from_legacy_backup", "primary_corrupt_recovered_from_backup", "primary_missing_recovered_from_backup", "incomplete_commit_preserved", "migrated_v0_to_v1":
		return status.Code
	default:
		return "state_recovered"
	}
}

func startupRecoverySource(status hoststate.StartupStatus) string {
	switch status.Source {
	case "primary", "backup", "migration", "initial":
		return status.Source
	default:
		return "none"
	}
}
