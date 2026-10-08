package configsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

var ErrEngineInvalid = errors.New("invalid config sync engine")

const (
	initialSyncRetryDelay = 100 * time.Millisecond
	maximumSyncRetryDelay = 2 * time.Second
)

type Syncer interface {
	Sync(context.Context, string) (PublishResult, error)
}

type StatusReporter interface {
	ReportStatus(context.Context, Status, int) error
}

type ManifestSource interface {
	CurrentManifest() Manifest
}

type EngineConfig struct {
	HomeRoot    string
	Descriptor  RuntimeDescriptor
	Syncer      Syncer
	Statuses    StatusReporter
	Diagnostics DiagnosticsSource
	Manifest    ManifestSource
	StatusPath  string
	Clock       func() time.Time
}

type Engine struct {
	mapping     *PathMapping
	homeRoot    string
	descriptor  RuntimeDescriptor
	syncer      Syncer
	statuses    StatusReporter
	diagnostics DiagnosticsSource
	manifest    ManifestSource
	statusPath  string
	clock       func() time.Time

	syncFailures       configSyncFailureObservation
	statusFileFailures configSyncFailureObservation
	statusAPIFailures  configSyncFailureObservation

	mu             sync.Mutex
	syncMu         sync.Mutex
	cancel         context.CancelFunc
	done           chan struct{}
	watcher        *fsnotify.Watcher
	remoteRevision string
	syncRevision   int64
	lastPush       time.Time
	dirtySince     time.Time
	status         Status
}

func (e *Engine) Apply(ctx context.Context) error {
	return e.syncNow(ctx)
}

func NewEngine(config EngineConfig) (*Engine, error) {
	if !canonicalAbsolutePath(config.HomeRoot) || config.Syncer == nil ||
		validateRuntimeDescriptor(config.Descriptor, Credential{
			EnvironmentID: config.Descriptor.EnvironmentID, MachineID: config.Descriptor.MachineID,
			AssignmentID: config.Descriptor.AssignmentID, AssignmentVersion: config.Descriptor.AssignmentVersion, WarningRevision: config.Descriptor.WarningRevision,
		}) != nil || (config.StatusPath != "" && !canonicalAbsolutePath(config.StatusPath)) {
		return nil, ErrEngineInvalid
	}
	if err := checkSafeAbsolutePath(config.HomeRoot); err != nil {
		return nil, errors.Join(ErrEngineInvalid, err)
	}
	info, err := os.Lstat(config.HomeRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.Join(ErrEngineInvalid, err)
	}
	resolved, err := filepath.EvalSymlinks(config.HomeRoot)
	if err != nil || !mappedPlatformPathsEqual(resolved, config.HomeRoot) {
		return nil, errors.Join(ErrEngineInvalid, err)
	}
	mapping, err := resolvePathRules(config.HomeRoot, config.Descriptor.PathRules, false)
	if err != nil {
		return nil, err
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	now := config.Clock().UTC()
	syncRevision := config.Descriptor.SyncRevisionFloor
	if config.StatusPath != "" {
		if previous, readErr := ReadStatus(config.StatusPath, config.Descriptor.Policy.SummaryLimit); readErr == nil &&
			previous.RepositoryID == config.Descriptor.RepositoryID &&
			previous.AssignmentID == config.Descriptor.AssignmentID &&
			previous.EnvironmentID == config.Descriptor.EnvironmentID &&
			previous.MachineID == config.Descriptor.MachineID &&
			previous.InstallationGeneration == config.Descriptor.InstallationGeneration {
			if previous.SyncRevision > syncRevision {
				syncRevision = previous.SyncRevision
			}
		}
	}
	return &Engine{
		mapping: mapping, homeRoot: config.HomeRoot, descriptor: config.Descriptor, syncer: config.Syncer,
		statuses: config.Statuses, statusPath: config.StatusPath, clock: config.Clock,
		diagnostics: config.Diagnostics, manifest: config.Manifest, syncRevision: syncRevision,
		status: Status{
			State: "restoring", Mode: config.Descriptor.Mode, RepositoryID: config.Descriptor.RepositoryID,
			AssignmentID: config.Descriptor.AssignmentID, EnvironmentID: config.Descriptor.EnvironmentID,
			MachineID: config.Descriptor.MachineID, WarningRevision: config.Descriptor.WarningRevision,
			InstallationGeneration: config.Descriptor.InstallationGeneration,
			PolicyRevision:         config.Descriptor.Policy.Revision,
			SyncRevision:           syncRevision, UpdatedAt: now,
		},
	}, nil
}

func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.cancel != nil {
		e.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	e.cancel, e.done = cancel, done
	e.mu.Unlock()
	started := false
	defer func() {
		if !started {
			cancel()
			e.mu.Lock()
			if e.done == done {
				e.cancel, e.done, e.watcher = nil, nil, nil
			}
			e.mu.Unlock()
			close(done)
		}
	}()
	// Source approval is checked by the real reconciler before any mapped watch
	// is opened. A stale projection cannot drive filesystem discovery.
	if err := e.syncNow(runCtx); err != nil && !errors.Is(err, ErrConfigConflict) {
		return err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := resetMappedWatches(watcher, e.mapping, e.descriptor.Policy, e.currentManifest()); err != nil {
		watcher.Close()
		return err
	}
	e.mu.Lock()
	if runCtx.Err() != nil || e.done != done {
		e.mu.Unlock()
		watcher.Close()
		return context.Canceled
	}
	e.watcher = watcher
	e.mu.Unlock()
	started = true
	go e.run(runCtx, done, watcher)
	return nil
}

// PauseConfiguration reports the file-edit pause after the supervisor joins
// the runtime, retaining the last reconciled state until approval resumes it.
func (e *Engine) PauseConfiguration(ctx context.Context, err error) {
	e.mu.Lock()
	e.syncRevision++
	e.status.SyncRevision = e.syncRevision
	e.status.UpdatedAt = e.clock().UTC()
	e.status.State = "pending"
	e.status.ErrorCode = "configuration_changed"
	e.status.RecoveryActions = []string{"apply_configuration"}
	if errors.Is(err, ErrSourceConfigInvalid) {
		e.status.State = "error"
		e.status.ErrorCode = "configuration_invalid"
		e.status.RecoveryActions = []string{"fix_configuration"}
	}
	e.mu.Unlock()
	e.report(ctx)
}

func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	cancel, done, watcher := e.cancel, e.done, e.watcher
	e.cancel, e.done, e.watcher = nil, nil, nil
	e.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		if watcher != nil {
			_ = watcher.Close()
		}
		return ctx.Err()
	}
	if watcher != nil {
		_ = watcher.Close()
	}
	if !FinalFlushAllowed(ctx) {
		return nil
	}
	flushTimeout := e.descriptor.Policy.ShutdownFlushTimeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < flushTimeout {
		flushTimeout = time.Until(deadline)
	}
	if flushTimeout <= 0 {
		return context.DeadlineExceeded
	}
	flushCtx, flushCancel := context.WithTimeout(ctx, flushTimeout)
	defer flushCancel()
	return e.syncNow(flushCtx)
}

func (e *Engine) run(ctx context.Context, done chan<- struct{}, watcher *fsnotify.Watcher) {
	defer close(done)
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	poll := time.NewTicker(e.descriptor.Policy.RemotePollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				if info, err := os.Lstat(event.Name); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
					if err := resetMappedWatches(watcher, e.mapping, e.descriptor.Policy, e.currentManifest()); err != nil {
						e.syncFailures.observe(ctx, "reconciliation", err)
					}
				}
			}
			if e.managedEvent(event.Name) {
				e.markDirty()
				resetTimer(debounce, e.nextDelay())
			}
		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return
			}
			if watchErr != nil {
				e.syncFailures.observe(ctx, "reconciliation", watchErr)
			}
			e.markDirty()
			resetTimer(debounce, e.descriptor.Policy.Debounce)
		case <-debounce.C:
			e.backgroundSync(ctx)
		case <-poll.C:
			e.backgroundSync(ctx)
		}
	}
}

func (e *Engine) backgroundSync(ctx context.Context) {
	if err := e.syncNow(ctx); err != nil {
		e.syncFailures.observe(ctx, "reconciliation", err)
		return
	}
	e.mu.Lock()
	usable := e.status.State == "healthy" || e.status.State == "warning" || e.status.State == "conflict"
	e.mu.Unlock()
	if usable {
		e.syncFailures.recovered(ctx)
	}
}

func (e *Engine) markDirty() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dirtySince.IsZero() {
		e.dirtySince = e.clock().UTC()
	}
}

func (e *Engine) nextDelay() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock().UTC()
	delay := e.descriptor.Policy.Debounce
	if !e.lastPush.IsZero() {
		if remaining := e.descriptor.Policy.MinimumPushInterval - now.Sub(e.lastPush); remaining > delay {
			delay = remaining
		}
	}
	if !e.dirtySince.IsZero() {
		if remaining := e.descriptor.Policy.MaximumDirtyDelay - now.Sub(e.dirtySince); remaining < delay {
			delay = remaining
		}
	}
	if delay < 0 {
		return 0
	}
	return delay
}

func (e *Engine) managedEvent(full string) bool {
	if !e.mapping.managedEvent(full, e.descriptor.Policy, e.currentManifest()) {
		return false
	}
	if observation, ok := e.manifest.(interface {
		ObservedLocalFile(string) (FileState, bool)
	}); ok {
		if name, ok := e.mapping.RepositoryPath(full); ok {
			if state, known := observation.ObservedLocalFile(name); known {
				if state.Hash == "" {
					_, err := os.Lstat(full)
					return !errors.Is(err, os.ErrNotExist)
				}
				_, err := e.mapping.read(name, state, e.descriptor.Policy.MaxFileBytes)
				return err != nil
			}
		}
	}
	return true
}

func (e *Engine) currentManifest() Manifest {
	if e.manifest == nil {
		return Manifest{}
	}
	return e.manifest.CurrentManifest()
}

func (e *Engine) syncNow(ctx context.Context) error {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.mu.Lock()
	remoteRevision := e.remoteRevision
	e.status.State = "syncing"
	now := e.clock().UTC()
	e.status.LastAttemptAt = &now
	e.status.UpdatedAt = now
	e.syncRevision++
	e.status.SyncRevision = e.syncRevision
	e.mu.Unlock()
	e.report(ctx)

	result, err := e.syncWithRetry(ctx, remoteRevision)
	e.mu.Lock()
	watcher := e.watcher
	e.mu.Unlock()
	if watcher != nil {
		if watchErr := resetMappedWatches(watcher, e.mapping, e.descriptor.Policy, e.currentManifest()); watchErr != nil {
			if err == nil {
				err = watchErr
			} else {
				err = errors.Join(err, watchErr)
			}
		}
	}
	diagnostics := ReconciliationDiagnostics{}
	if e.diagnostics != nil {
		diagnostics = e.diagnostics.Diagnostics()
	}
	e.mu.Lock()
	now = e.clock().UTC()
	e.status.UpdatedAt = now
	e.status.Skipped = diagnostics.Skipped
	e.status.Conflicts = diagnostics.Conflicts
	e.status.ManifestHealth = diagnostics.ManifestHealth
	e.status.ManifestRevision = diagnostics.ManifestRevision
	e.status.ManagedPathCount = diagnostics.ManagedPathCount
	e.status.PendingCleanPathCount = diagnostics.PendingCleanPathCount
	if diagnostics.LastAppliedRevision != "" {
		e.status.LastAppliedRevision = diagnostics.LastAppliedRevision
	}
	if diagnostics.LastPublishedRevision != "" {
		e.status.LastPublishedRevision = diagnostics.LastPublishedRevision
	}
	if result.RemoteRevision != "" {
		e.remoteRevision = result.RemoteRevision
		e.status.RemoteRevision = result.RemoteRevision
	}
	if result.Review != nil {
		e.status.Review = boundPathSummaries(result.Review, e.descriptor.Policy.SummaryLimit)
	}
	if result.Landed {
		// Publication progress survives cleanup/acknowledgement failures.
		e.lastPush = now
		e.dirtySince = time.Time{}
		e.status.LastSuccessfulAt = &now
	}
	switch {
	case result.Landed && err != nil:
		e.status.State = "warning"
		e.status.ErrorCode = "repository_unavailable"
		e.status.RecoveryActions = []string{"check_status"}
	case err == nil && result.Landed && len(diagnostics.Conflicts) > 0:
		e.remoteRevision = result.RemoteRevision
		e.lastPush = now
		e.dirtySince = time.Time{}
		e.status.State = "conflict"
		e.status.RemoteRevision = result.RemoteRevision
		e.status.LastSuccessfulAt = &now
		e.status.ErrorCode = "config_conflict"
		e.status.RecoveryActions = []string{"keep_local", "keep_remote"}
	case err == nil && result.Landed:
		e.remoteRevision = result.RemoteRevision
		e.lastPush = now
		e.dirtySince = time.Time{}
		e.status.State = "healthy"
		if len(diagnostics.Skipped) > 0 {
			e.status.State = "warning"
		}
		e.status.RemoteRevision = result.RemoteRevision
		e.status.LastSuccessfulAt = &now
		e.status.ErrorCode = ""
		e.status.RecoveryActions = nil
	case errors.Is(err, ErrSyncUncertain):
		e.status.State = "sync_uncertain"
		e.status.ErrorCode = "sync_uncertain"
		e.status.RecoveryActions = []string{"observe_remote"}
	case errors.Is(err, ErrRepositoryUnavailable):
		e.status.State = "offline"
		e.status.ErrorCode = "repository_unavailable"
		e.status.RecoveryActions = []string{"check_repository_access", "retry"}
	case errors.Is(err, ErrConfigConflict):
		e.status.State = "conflict"
		e.status.ErrorCode = "config_conflict"
		e.status.RecoveryActions = []string{"keep_local", "keep_remote"}
	case errors.Is(err, ErrWritesDisabled):
		e.status.State = "warning"
		e.status.ErrorCode = "writes_disabled"
		e.status.RecoveryActions = []string{"wait_for_rollout"}
	case errors.Is(err, ErrLeaseBusy):
		e.status.State = "pending"
		e.status.ErrorCode = "lease_busy"
		e.status.RecoveryActions = []string{"wait_for_lease"}
	case errors.Is(err, ErrLeaseLost):
		e.status.State = "pending"
		e.status.ErrorCode = "lease_lost"
		e.status.RecoveryActions = []string{"retry"}
	case errors.Is(err, ErrRemoteRevisionChanged):
		e.status.State = "pending"
		e.status.ErrorCode = "remote_revision_changed"
		e.status.RecoveryActions = []string{"retry"}
	case errors.Is(err, ErrReviewRequired):
		e.status.State = "pending"
		e.status.ErrorCode = "review_required"
		e.status.RecoveryActions = []string{"review_revision"}
	case errors.Is(err, ErrConfigurationChanged):
		e.status.State = "pending"
		e.status.ErrorCode = "configuration_changed"
		e.status.RecoveryActions = []string{"apply_configuration"}
	case errors.Is(err, ErrSourceConfigInvalid):
		e.status.State = "error"
		e.status.ErrorCode = "configuration_invalid"
		e.status.RecoveryActions = []string{"fix_configuration"}
	case errors.Is(err, ErrRepositoryCredentials):
		e.status.State = "error"
		e.status.ErrorCode = "repository_credentials_required"
		e.status.RecoveryActions = []string{"configure_repository_credentials"}
	case errors.Is(err, ErrPathRuleInvalid):
		e.status.State = "error"
		e.status.ErrorCode = "config_path_invalid"
		e.status.RecoveryActions = []string{"fix_path_rules"}
	case errors.Is(err, ErrManifestMissing):
		e.status.State = "error"
		e.status.ErrorCode = "manifest_missing"
		e.status.RecoveryActions = []string{"fix_manifest"}
	case errors.Is(err, ErrManifestInvalid), errors.Is(err, ErrManifestUnsafePath):
		e.status.State = "error"
		e.status.ErrorCode = "manifest_invalid"
		e.status.RecoveryActions = []string{"fix_manifest"}
	case expectedAuthorizationDenial(err):
		e.status.State = "revoked"
		e.status.ErrorCode = "credential_expired"
	default:
		e.status.State = "error"
		e.status.ErrorCode = "repository_unavailable"
	}
	e.mu.Unlock()
	e.report(ctx)
	return err
}

func (e *Engine) syncWithRetry(ctx context.Context, remoteRevision string) (PublishResult, error) {
	var lastErr error
	for attempt := 0; attempt < e.descriptor.Policy.RetryLimit; attempt++ {
		result, err := e.syncer.Sync(ctx, remoteRevision)
		if result.Landed || result.Uncertain || err == nil || !retryableSyncError(err) || attempt+1 == e.descriptor.Policy.RetryLimit {
			return result, err
		}
		lastErr = err
		delay := syncRetryDelay(attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return PublishResult{}, errors.Join(ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
	return PublishResult{}, ErrEngineInvalid
}

func retryableSyncError(err error) bool {
	return err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, ErrConfigConflict) &&
		!errors.Is(err, ErrSyncUncertain) &&
		!errors.Is(err, ErrAuthorization) &&
		!errors.Is(err, ErrWritesDisabled) &&
		!errors.Is(err, ErrReviewRequired) &&
		!errors.Is(err, ErrManifestMissing) &&
		!errors.Is(err, ErrManifestInvalid) &&
		!errors.Is(err, ErrManifestUnsafePath) &&
		!errors.Is(err, ErrBaselineInvalid) &&
		!errors.Is(err, ErrPathRuleInvalid) &&
		(!errors.Is(err, ErrRepositoryCredentials) || errors.Is(err, ErrRepositoryUnavailable)) &&
		!errors.Is(err, ErrConfigurationChanged) &&
		!errors.Is(err, ErrSourceConfigInvalid)
}

func syncRetryDelay(attempt int) time.Duration {
	delay := initialSyncRetryDelay
	for index := 0; index < attempt && delay < maximumSyncRetryDelay; index++ {
		delay *= 2
	}
	if delay > maximumSyncRetryDelay {
		return maximumSyncRetryDelay
	}
	return delay
}

func (e *Engine) report(ctx context.Context) {
	e.mu.Lock()
	status := e.status
	e.mu.Unlock()
	if e.statusPath != "" {
		if err := WriteStatus(e.statusPath, status, e.descriptor.Policy.SummaryLimit); err != nil {
			e.statusFileFailures.observe(ctx, "reconciliation", err)
		} else {
			e.statusFileFailures.recovered(ctx)
		}
	}
	if e.statuses != nil {
		if err := e.statuses.ReportStatus(ctx, status, e.descriptor.Policy.SummaryLimit); err != nil {
			e.statusAPIFailures.observe(ctx, "control_request", err)
		} else {
			e.statusAPIFailures.recovered(ctx)
		}
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}
