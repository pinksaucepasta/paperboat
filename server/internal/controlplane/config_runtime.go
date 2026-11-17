package controlplane

import (
	"context"
	"database/sql"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/shared/configsync"
)

var ErrConfigRuntimeInvalid = errors.New("config runtime request is invalid")

type ConfigRuntimePolicy = configsync.RuntimePolicy

type ConfigRuntimeDescriptor = configsync.RuntimeDescriptor

type ConfigRuntimeService struct {
	store  *db.DB
	leases *ConfigLeaseService
	policy config.ConfigSync
}

func NewConfigRuntimeService(store *db.DB, leases *ConfigLeaseService, policy config.ConfigSync) *ConfigRuntimeService {
	return &ConfigRuntimeService{store: store, leases: leases, policy: policy}
}

func (s *ConfigRuntimeService) Get(ctx context.Context, identityToken, credential string, proof, body []byte, method, path string) (ConfigRuntimeDescriptor, error) {
	if s == nil || s.store == nil || s.leases == nil {
		return ConfigRuntimeDescriptor{}, ErrConfigRuntimeInvalid
	}
	holder, err := s.leases.Authenticate(ctx, identityToken, credential, proof, body, method, path)
	if err != nil {
		if errors.Is(err, ErrConfigLeaseInvalid) {
			return ConfigRuntimeDescriptor{}, ErrConfigRuntimeInvalid
		}
		return ConfigRuntimeDescriptor{}, err
	}
	assignment, err := s.store.Queries().GetControlConfigAssignment(ctx, holder.EnvironmentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConfigRuntimeDescriptor{}, ErrConfigRuntimeInvalid
		}
		return ConfigRuntimeDescriptor{}, err
	}
	if !assignment.WarningRevision.Valid {
		return ConfigRuntimeDescriptor{}, ErrConfigRuntimeInvalid
	}
	syncRevisionFloor, err := s.store.Queries().GetControlConfigSyncRevision(ctx, dbsqlc.GetControlConfigSyncRevisionParams{
		EnvironmentID: holder.EnvironmentID, AssignmentID: holder.AssignmentID,
		MachineID: holder.MachineID, InstallationGeneration: holder.InstallationGeneration,
	})
	if errors.Is(err, sql.ErrNoRows) {
		syncRevisionFloor = 0
	} else if err != nil {
		return ConfigRuntimeDescriptor{}, err
	}
	machine, err := s.store.Queries().GetActiveUserMachineForControl(ctx, holder.MachineID)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigRuntimeDescriptor{}, ErrConfigRuntimeInvalid
	} else if err != nil {
		return ConfigRuntimeDescriptor{}, err
	}
	return ConfigRuntimeDescriptor{
		WriteMode: s.policy.Mode, Mode: configsync.AssignmentMode(assignment.Mode), PathRules: ConfigAssignmentRules(assignment.PathRules), RepositoryBindings: ConfigAssignmentBindings(assignment.RepositoryBindings), AssignmentVersion: assignment.Version, ConfigurationRevision: assignment.ConfigurationRevision,
		RepositoryID: statusRepositoryID(assignment), PullRepositoryID: pullRepositoryID(assignment), PushRepositoryID: pushRepositoryID(assignment),
		AutomaticUpdates: assignment.AutomaticUpdates, ApprovedPullRevision: nullStringValue(assignment.ApprovedPullRevision),
		AssignmentID: holder.AssignmentID, EnvironmentID: holder.EnvironmentID,
		MachineID: holder.MachineID, MachineAlias: machine.Alias, WarningRevision: assignment.WarningRevision.String,
		InstallationGeneration: holder.InstallationGeneration, SyncRevisionFloor: syncRevisionFloor,
		Policy: ConfigRuntimePolicy{
			Format: "paperboat-config-plaintext-v1", Revision: s.policy.PolicyRevision,
			ManifestContract: s.policy.ManifestContract, ManifestMaxBytes: s.policy.ManifestMaxBytes,
			ManifestMaxLines: s.policy.ManifestMaxLines, ManifestMaxPatternBytes: s.policy.ManifestMaxPatternBytes,
			MaxFileBytes: s.policy.MaxFileBytes, MaxBatchBytes: s.policy.MaxBatchBytes,
			Debounce: s.policy.Debounce, MinimumPushInterval: s.policy.MinPushInterval,
			MaximumDirtyDelay: s.policy.MaxDirtyDelay, RemotePollInterval: s.policy.RemotePollInterval,
			RetryLimit: s.policy.RetryLimit, ShutdownFlushTimeout: s.policy.ShutdownFlushTimeout,
			SummaryLimit: s.policy.SummaryLimit,
		},
	}, nil
}

func pullRepositoryID(assignment dbsqlc.ControlConfigAssignment) string {
	if assignment.Mode == ConfigModePushOnly {
		return ""
	}
	return nullStringValue(assignment.RepositoryID)
}

func pushRepositoryID(assignment dbsqlc.ControlConfigAssignment) string {
	if assignment.Mode == ConfigModePullOnly {
		return ""
	}
	if assignment.PushRepositoryID.Valid {
		return assignment.PushRepositoryID.String
	}
	return nullStringValue(assignment.RepositoryID)
}

func statusRepositoryID(assignment dbsqlc.ControlConfigAssignment) string {
	if pull := pullRepositoryID(assignment); pull != "" {
		return pull
	}
	return pushRepositoryID(assignment)
}

func nullStringValue(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}
