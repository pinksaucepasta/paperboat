//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machinecontrol"
)

type productionConfigSyncConfig struct {
	ControlURL      string
	ControlHost     string
	RepositoryHosts []string
	HomeRoot        string
	StateRoot       string
	Identities      configsync.TokenSource
	Proofs          configsync.ProofSource
	OperationID     configsync.OperationIDSource
	Transport       http.RoundTripper
}

type ProductionConfigWorkerConfig struct {
	ControlURL      string
	StateRoot       string
	HomeRoot        string
	RepositoryHosts []string
	Transport       http.RoundTripper
}

func RunProductionConfigWorker(ctx context.Context, config ProductionConfigWorkerConfig) error {
	controlURL, err := url.Parse(strings.TrimSpace(config.ControlURL))
	if err != nil || controlURL.Scheme != "https" || controlURL.Hostname() == "" || !filepath.IsAbs(config.StateRoot) || !filepath.IsAbs(config.HomeRoot) {
		return ErrProductionInvalid
	}
	identities, err := machinecontrol.NewSource(machinecontrol.Config{ControlURL: controlURL.String(), StateRoot: config.StateRoot, Transport: config.Transport})
	if err != nil {
		return err
	}
	service, err := newProductionConfigSync(productionConfigSyncConfig{
		ControlURL: controlURL.String(), ControlHost: controlURL.Hostname(), RepositoryHosts: config.RepositoryHosts,
		HomeRoot: config.HomeRoot, StateRoot: config.StateRoot,
		Identities: identities, Proofs: identities, OperationID: randomProductionOperationID, Transport: config.Transport,
	})
	if err != nil {
		return err
	}
	if err := service.Start(ctx); err != nil {
		return err
	}
	// Start owns an asynchronous component. The standalone worker owns the
	// process lifetime and must join it before the service process exits.
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return service.Shutdown(shutdownCtx)
}

func randomProductionOperationID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return "operation_" + id.String(), nil
}

func newProductionConfigSync(config productionConfigSyncConfig) (*configsync.Supervisor, error) {
	client, err := configsync.NewControlClient(configsync.ControlClientConfig{
		BaseURL: config.ControlURL, AllowedHosts: []string{config.ControlHost},
		RepositoryHosts: config.RepositoryHosts, Identities: config.Identities,
		Proofs: config.Proofs, OperationID: config.OperationID, Transport: config.Transport,
	})
	if err != nil {
		return nil, err
	}
	machinePath, err := configsync.DefaultMachineSourcePath()
	if err != nil {
		return nil, err
	}
	loadMachine := func() (configsync.SourceConfig, error) {
		return configsync.LoadSourceConfig(machinePath, configsync.DefaultSourceConfigLimits())
	}
	return configsync.NewSupervisor(configsync.SupervisorConfig{
		Credentials: client,
		ConfigRevision: func(ctx context.Context) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			source, err := loadMachine()
			if err != nil {
				return "", err
			}
			data, err := json.Marshal(source)
			if err != nil {
				return "", err
			}
			hash := sha256.Sum256(data)
			return hex.EncodeToString(hash[:]), nil
		},
		Factory: func(ctx context.Context, credential configsync.Credential) (configsync.Runtime, error) {
			machineSource, err := loadMachine()
			if err != nil {
				return nil, err
			}
			descriptor, err := client.RuntimeDescriptor(ctx)
			if err != nil {
				return nil, err
			}
			if descriptor.AssignmentID != credential.AssignmentID ||
				descriptor.EnvironmentID != credential.EnvironmentID ||
				descriptor.MachineID != credential.MachineID ||
				descriptor.WarningRevision != credential.WarningRevision {
				return nil, configsync.ErrAuthorization
			}
			descriptor.Policy.AbsoluteRuntimeExclusionRoots = append(descriptor.Policy.AbsoluteRuntimeExclusionRoots, machinePath)
			descriptor, err = protectConfigSyncRuntimeState(descriptor, config.HomeRoot, config.StateRoot)
			if err != nil {
				return nil, err
			}
			hash := sha256.Sum256([]byte(descriptor.AssignmentID))
			assignmentRoot := filepath.Join(config.StateRoot, "config-sync", hex.EncodeToString(hash[:16]))
			if err := os.MkdirAll(assignmentRoot, 0o700); err != nil {
				return nil, err
			}
			assignmentRoot, err = filepath.EvalSymlinks(assignmentRoot)
			if err != nil {
				return nil, err
			}
			credentialRoot := filepath.Join(config.StateRoot, "config-sync", "repository-credentials")
			if err := configsync.EnsureRepositoryCredentialRoot(credentialRoot); err != nil {
				return nil, err
			}
			repository, reconciler, err := productionConfigRepository(config.HomeRoot, assignmentRoot, credentialRoot, descriptor, machineSource, client)
			if err != nil {
				return nil, err
			}
			publisher, err := configsync.NewPublisher(configsync.PublisherConfig{
				Authority: client, Repository: repository, AutomaticUpdates: descriptor.AutomaticUpdates,
				ApprovedRevision: descriptor.ApprovedPullRevision, RequireReview: descriptor.PullRepositoryID != "",
			})
			if err != nil {
				return nil, err
			}
			return configsync.NewEngine(configsync.EngineConfig{
				HomeRoot: config.HomeRoot, Descriptor: descriptor, Syncer: publisher, Statuses: client,
				Diagnostics: reconciler, Manifest: reconciler,
				StatusPath: filepath.Join(assignmentRoot, "status.json"),
			})
		},
	})
}

type productionConfigReconciler interface {
	configsync.DiagnosticsSource
	configsync.ManifestSource
}

func productionConfigRepository(homeRoot, assignmentRoot, credentialRoot string, descriptor configsync.RuntimeDescriptor, machineSource configsync.SourceConfig, client *configsync.ControlClient) (configsync.Repository, productionConfigReconciler, error) {
	if descriptor.PullRepositoryID == "" && descriptor.PushRepositoryID == "" {
		if descriptor.Mode != configsync.ModePushOnly {
			descriptor.PullRepositoryID = descriptor.RepositoryID
		}
		if descriptor.Mode != configsync.ModePullOnly {
			descriptor.PushRepositoryID = descriptor.RepositoryID
		}
	}
	primaryDirection := "pull"
	if descriptor.PullRepositoryID == "" {
		primaryDirection = "push"
	}
	sharedSource := func(ctx context.Context) (configsync.SourceConfig, error) {
		access, err := (configsync.DirectionalRepositoryAccess{Client: client, Direction: primaryDirection}).RepositoryAccess(ctx)
		if err != nil {
			return configsync.SourceConfig{}, err
		}
		source, _, err := configsync.ReadSharedAuthorizedRepositoryConfig(ctx, filepath.Join(assignmentRoot, "source-read"), credentialRoot, access, configsync.DefaultSourceConfigLimits())
		return source, err
	}
	makeRepository := func(name, repositoryID string, mode configsync.AssignmentMode, direction string) (*configsync.GitRepository, *configsync.PlaintextWorkspaceReconciler, error) {
		stateRoot := filepath.Join(assignmentRoot, name)
		if err := os.MkdirAll(stateRoot, 0o700); err != nil {
			return nil, nil, err
		}
		child := descriptor
		child.RepositoryID, child.Mode = repositoryID, mode
		reconciler, err := configsync.NewPlaintextWorkspaceReconciler(configsync.WorkspaceReconcilerConfig{HomeRoot: homeRoot, StateRoot: stateRoot, ComparisonStatusPath: filepath.Join(assignmentRoot, "status.json"), Descriptor: child, MachineSource: machineSource, ApprovedConfiguration: &descriptor, SharedSource: sharedSource, Resolutions: client})
		if err != nil {
			return nil, nil, err
		}
		repository, err := configsync.NewGitRepository(configsync.GitRepositoryConfig{Root: filepath.Join(stateRoot, "repository"), CredentialRoot: credentialRoot, Access: configsync.DirectionalRepositoryAccess{Client: client, Direction: direction}, Reconciler: reconciler, PushTarget: direction == "push"})
		return repository, reconciler, err
	}
	if descriptor.Mode == configsync.ModePullOnly {
		return makeRepository("pull", descriptor.PullRepositoryID, configsync.ModePullOnly, "pull")
	}
	if descriptor.Mode == configsync.ModePushOnly {
		return makeRepository("push", descriptor.PushRepositoryID, configsync.ModePushOnly, "push")
	}
	pull, pullReconciler, err := makeRepository("pull", descriptor.PullRepositoryID, configsync.ModePullOnly, "pull")
	if err != nil {
		return nil, nil, err
	}
	push, pushReconciler, err := makeRepository("push", descriptor.PushRepositoryID, configsync.ModePushOnly, "push")
	if err != nil {
		return nil, nil, err
	}
	return &configsync.SplitRepository{Pull: pull, Push: push}, combinedConfigDiagnostics{pullReconciler, pushReconciler}, nil
}

type combinedConfigDiagnostics struct {
	pull, push *configsync.PlaintextWorkspaceReconciler
}

func (d combinedConfigDiagnostics) Diagnostics() configsync.ReconciliationDiagnostics {
	pull, push := d.pull.Diagnostics(), d.push.Diagnostics()
	push.LastAppliedRevision = pull.LastAppliedRevision
	push.Skipped = append(pull.Skipped, push.Skipped...)
	return push
}
func (d combinedConfigDiagnostics) CurrentManifest() configsync.Manifest {
	return d.pull.CurrentManifest()
}

func (d combinedConfigDiagnostics) ObservedLocalFile(name string) (configsync.FileState, bool) {
	if state, known := d.push.ObservedLocalFile(name); known {
		return state, true
	}
	return d.pull.ObservedLocalFile(name)
}

func protectConfigSyncRuntimeState(
	descriptor configsync.RuntimeDescriptor,
	homeRoot string,
	stateRoot string,
) (configsync.RuntimeDescriptor, error) {
	descriptor.Policy.AbsoluteRuntimeExclusionRoots = append(descriptor.Policy.AbsoluteRuntimeExclusionRoots, filepath.Clean(stateRoot))
	relative, err := filepath.Rel(homeRoot, stateRoot)
	if err != nil {
		return configsync.RuntimeDescriptor{}, errors.Join(ErrProductionInvalid, err)
	}
	if relative == "." {
		return configsync.RuntimeDescriptor{}, errors.Join(ErrProductionInvalid, errors.New("config sync state root cannot be the managed home"))
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return descriptor, nil
	}
	relative = filepath.ToSlash(relative)
	for _, existing := range descriptor.Policy.RuntimeExclusionRoots {
		if existing == relative {
			return descriptor, nil
		}
	}
	descriptor.Policy.RuntimeExclusionRoots = append(descriptor.Policy.RuntimeExclusionRoots, relative)
	return descriptor, nil
}
