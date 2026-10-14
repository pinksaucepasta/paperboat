//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
)

func productionConfigComparison(config productionConfigSyncConfig) (server.ConfigComparisonReader, error) {
	client, err := configsync.NewControlClient(configsync.ControlClientConfig{BaseURL: config.ControlURL, AllowedHosts: []string{config.ControlHost}, RepositoryHosts: config.RepositoryHosts, Identities: config.Identities, Proofs: config.Proofs, OperationID: config.OperationID, Transport: config.Transport})
	if err != nil {
		return nil, err
	}
	machinePath, err := configsync.DefaultMachineSourcePath()
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, request configsync.ConflictComparisonRequest) (configsync.ConflictComparison, error) {
		descriptor, err := client.RuntimeDescriptor(ctx)
		if err != nil {
			return configsync.ConflictComparison{}, err
		}
		if descriptor.AssignmentID != request.AssignmentID || descriptor.AssignmentVersion != request.AssignmentVersion {
			return configsync.ConflictComparison{}, configsync.ErrConflictComparisonStale
		}
		descriptor.Policy.AbsoluteRuntimeExclusionRoots = append(descriptor.Policy.AbsoluteRuntimeExclusionRoots, machinePath)
		descriptor, err = protectConfigSyncRuntimeState(descriptor, config.HomeRoot, config.StateRoot)
		if err != nil {
			return configsync.ConflictComparison{}, err
		}
		source, err := configsync.LoadSourceConfig(machinePath, configsync.DefaultSourceConfigLimits())
		if err != nil {
			return configsync.ConflictComparison{}, err
		}
		hash := sha256.Sum256([]byte(request.AssignmentID))
		assignmentRoot := filepath.Join(config.StateRoot, "config-sync", hex.EncodeToString(hash[:16]))
		credentialRoot := filepath.Join(config.StateRoot, "config-sync", "repository-credentials")
		repository, _, err := productionConfigRepository(config.HomeRoot, assignmentRoot, credentialRoot, descriptor, source, client)
		if err != nil {
			return configsync.ConflictComparison{}, err
		}
		reader, ok := repository.(interface {
			CompareConflict(context.Context, configsync.ConflictComparisonRequest) (configsync.ConflictComparison, error)
		})
		if ok {
			return reader.CompareConflict(ctx, request)
		}
		if split, ok := repository.(*configsync.SplitRepository); ok {
			for _, candidate := range []configsync.Repository{split.Pull, split.Push} {
				if reader, ok := candidate.(interface {
					CompareConflict(context.Context, configsync.ConflictComparisonRequest) (configsync.ConflictComparison, error)
				}); ok {
					result, err := reader.CompareConflict(ctx, request)
					if !errors.Is(err, configsync.ErrConflictComparisonStale) {
						return result, err
					}
				}
			}
		}
		return configsync.ConflictComparison{}, configsync.ErrConflictComparisonStale
	}, nil
}
