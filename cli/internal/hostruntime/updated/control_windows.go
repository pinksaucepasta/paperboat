//go:build windows

package updated

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseeligibility"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/selfupdate"
)

var ErrWindowsActivationUnavailable = errors.New("Windows updater activation is unavailable")

var windowsActivationHandoffDelay = 2 * time.Second
var startWindowsActivator = startWindowsActivatorService

// resumeWindowsActivationForController is a seam for the controller's
// recovery path. Keeping the startup and live-controller paths on the same
// implementation is important: both paths must perform the SCM owner check
// before handing the journal back to the one-shot activator.
var resumeWindowsActivationForController = resumeWindowsActivation
var loadWindowsActivationJournalForController = loadWindowsActivationJournal

type windowsController struct {
	activeVersion string
	ownerSID      string
	socketPath    string
	resolve       workerupdate.Resolver
	scheduler     *autoupdate.Scheduler
	mu            sync.Mutex
	checkMu       sync.Mutex
	activationMu  sync.Mutex
	config        WindowsConfig
	source        workerupdate.TUFSource
	handoff       chan struct{}
	handoffOnce   sync.Once
}

func newWindowsController(config WindowsConfig) (*windowsController, error) {
	resolve := config.ResolveRelease
	var source workerupdate.TUFSource
	if resolve == nil {
		var err error
		source, err = newWindowsTUFSource(config)
		if err != nil {
			return nil, err
		}
		resolve = source.Resolve
	}
	controller := &windowsController{
		activeVersion: config.ActiveVersion,
		ownerSID:      config.OwnerSID,
		socketPath:    config.ControlSocket,
		resolve:       resolve,
		config:        config,
		source:        source,
		handoff:       make(chan struct{}),
	}
	scheduler, err := autoupdate.New(autoupdate.Config{Check: controller.automaticCheck,
		NextCheck: func(now, next time.Time) time.Time {
			return nextMachineUpdateCheck(config.StateRoot, config.AutomaticChecks, now, next)
		},
	})
	if err != nil {
		return nil, err
	}
	controller.scheduler = scheduler
	return controller, nil
}

func newWindowsTUFSource(config WindowsConfig) (workerupdate.TUFSource, error) {
	token, err := os.ReadFile(config.TokenFile)
	if err != nil {
		return workerupdate.TUFSource{}, err
	}
	client, err := hostdproto.NewClient(config.HostdSocket, token, 31*time.Minute)
	clear(token)
	if err != nil {
		return workerupdate.TUFSource{}, err
	}
	deferral, err := releaseeligibility.NewFileStore(filepath.Join(config.StateRoot, "deferral.json"))
	if err != nil {
		return workerupdate.TUFSource{}, err
	}
	return workerupdate.TUFSource{RepositoryURL: config.RepositoryURL, StateRoot: filepath.Join(config.StateRoot, "tuf"), MachineID: config.MachineID, FailureDomain: workerupdate.HostdFailureDomainSource{Client: client, MachineID: config.MachineID}, Deferral: deferral}, nil
}

func (c *windowsController) tufSource() (workerupdate.TUFSource, error) {
	if c == nil {
		return workerupdate.TUFSource{}, ErrWindowsActivationUnavailable
	}
	if c.source.RepositoryURL != "" {
		return c.source, nil
	}
	return newWindowsTUFSource(c.config)
}

func (c *windowsController) run(ctx context.Context, ready func() error) error {
	if c == nil || c.scheduler == nil || !validPipePath(c.socketPath) {
		return ErrInvalidWindowsConfig
	}
	listener, err := winio.ListenPipe(c.socketPath, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GRGW;;;SY)(A;;GRGW;;;" + c.ownerSID + ")",
		InputBufferSize:    16 << 10,
		OutputBufferSize:   16 << 10,
	})
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() {
		select {
		case <-ctx.Done():
		case <-c.handoff:
		}
		_ = listener.Close()
	}()
	go func() { _ = c.scheduler.Run(ctx) }()
	if ready != nil {
		if err := ready(); err != nil {
			return err
		}
	}
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || activationRequested(c.handoff) || errors.Is(acceptErr, net.ErrClosed) || errors.Is(acceptErr, winio.ErrPipeListenerClosed) {
				return nil
			}
			return acceptErr
		}
		activationPending, _ := c.handle(connection)
		_ = connection.Close()
		if activationPending {
			delay := windowsActivationHandoffDelay
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
			}
			if err := startWindowsActivator(c.config.OwnerSID); err != nil {
				continue
			}
			c.handoffOnce.Do(func() { close(c.handoff) })
			return nil
		}
	}
}

func (c *windowsController) handle(connection net.Conn) (bool, error) {
	if connection == nil {
		return false, ErrInvalidControl
	}
	_ = connection.SetDeadline(time.Now().Add(maxUpdateControlTimeout))
	reader := bufio.NewReaderSize(io.LimitReader(connection, (4<<10)+1), (4<<10)+1)
	body, err := reader.ReadBytes('\n')
	if err != nil || len(body) == 0 || len(body) > 4<<10 {
		return false, json.NewEncoder(connection).Encode(ControlResponse{Schema: ControlProtocolV1, Status: "error", ErrorCode: "invalid_request"})
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request ControlRequest
	var extra any
	if decoder.Decode(&request) != nil || decoder.Decode(&extra) != io.EOF || request.Schema != ControlProtocolV1 || !validControlRequest(request) {
		return false, json.NewEncoder(connection).Encode(ControlResponse{Schema: ControlProtocolV1, Status: "error", ErrorCode: "invalid_request"})
	}
	response, invokeErr := c.invoke(context.Background(), request)
	if invokeErr != nil {
		response.Schema = ControlProtocolV1
		response.Status = "error"
		response.ErrorCode = controlErrorCodeWindows(invokeErr)
		response.ErrorMessage = boundedControlErrorMessage(invokeErr)
	}
	if response.Schema == "" {
		response.Schema = ControlProtocolV1
	}
	if response.Status == "" {
		response.Status = "ok"
	}
	if err := json.NewEncoder(connection).Encode(response); err != nil {
		return false, err
	}
	return request.Operation == "install" && response.Pending && invokeErr == nil, nil
}

func (c *windowsController) invoke(ctx context.Context, request ControlRequest) (ControlResponse, error) {
	return c.invokeWithAutomatic(ctx, request, false)
}

func (c *windowsController) invokeWithAutomatic(ctx context.Context, request ControlRequest, automatic bool) (ControlResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	response := ControlResponse{Schema: ControlProtocolV1, Status: "ok", Version: c.activeVersion, Observation: c.scheduler.Snapshot()}
	if request.Operation == "settings" {
		settings, err := machineUpdateSettings(c.config.StateRoot, c.config.AutomaticChecks, request.Settings)
		if err != nil {
			return response, err
		}
		response.Settings = &settings
		if settings.Enabled {
			response.NextMaintenanceAt = settings.NextMaintenance(time.Now())
		}
		if request.Settings != nil {
			c.scheduler.Wake()
		}
		return response, nil
	}
	if err := populateMachineSettings(&response, c.config.StateRoot, c.config.AutomaticChecks); err != nil {
		return response, err
	}
	// Settings can change after the scheduler's initial read or its download.
	// Recheck under the same lock as activation before accepting an automatic
	// install, so opting out cannot race the scheduler into a cutover.
	if automatic && response.Settings != nil && !response.Settings.Enabled {
		return response, nil
	}
	blocked, blockErr := c.activationBlockedReadOnlyContext(ctx)
	if blockErr != nil {
		return response, blockErr
	}
	if blocked && request.Operation == "install" {
		journal, err := loadWindowsActivationJournalForController(c.config)
		if err != nil {
			return response, err
		}
		if request.ApprovalID != journal.Candidate.ID || journal.ApprovedCandidateID != request.ApprovalID {
			return response, workerupdate.ErrApprovalRequired
		}
		blocked, blockErr = c.activationBlockedContext(ctx)
		if blockErr != nil {
			return response, blockErr
		}
	}
	if blocked && request.Operation != "status" {
		return response, ErrWindowsActivationUnavailable
	}
	switch request.Operation {
	case "status":
		if journal, err := loadWindowsActivationJournalForController(c.config); err == nil {
			response.Transaction = windowsTransactionState(journal)
			if journal.Stage == windowsActivationAwaitingApproval {
				if err := verifyWindowsPreparedCandidate(ctx, journal); err != nil {
					return response, err
				}
				candidate := journal.Candidate
				response.Candidate = &candidate
			}

			response.Pending = journal.Stage != windowsActivationAwaitingApproval && journal.Stage != windowsActivationCommitted && journal.Stage != windowsActivationRolledBack
			response.Updated = journal.Version == c.activeVersion && journal.Stage == windowsActivationCommitted
			if journal.PreviousVersion == c.activeVersion {
				switch journal.Stage {
				case windowsActivationRolledBack:
					response.ActivationFailure = "activation_failed"
				case windowsActivationRollingBack, windowsActivationRollbackReady:
					response.ActivationFailure = "recovery_pending"
				}
			}
		}
		return response, nil
	case "check":
		// A manual check is read-only. Calling Scheduler.CheckNow here used the
		// automatic activation callback and could stage, stop services, and roll
		// back a release merely because the user asked what version was current.
		c.checkMu.Lock()
		result, err := c.scheduler.ObserveCheck(ctx, func(ctx context.Context) (autoupdate.Result, error) {
			workerResult, resolveErr := resolveRelease(ctx, c.activeVersion, c.resolve)
			return autoupdate.Result{Version: workerResult.Version}, resolveErr
		})
		c.checkMu.Unlock()
		response.Version, response.Updated, response.Observation = result.Version, false, c.scheduler.Snapshot()
		return response, err
	case "download", "install":
		if c.config.RepositoryURL == "" {
			return response, ErrWindowsActivationUnavailable
		}
		c.checkMu.Lock()
		defer c.checkMu.Unlock()
		source, err := c.tufSource()
		if err != nil {
			return response, err
		}
		resolve := source.ResolveManual
		if automatic {
			resolve = source.Resolve
		}
		release, found, err := resolve(ctx)
		if err != nil {
			return response, err
		}
		if !found {
			if automatic {
				return response, nil
			}
			return response, workerupdate.ErrPreparedCandidate
		}
		comparison, err := compareWindowsInstalledVersion(release.Version, c.activeVersion, c.config.Source)
		if err != nil || comparison < 0 {
			return response, workerupdate.ErrInvalidRelease
		}
		if comparison == 0 {
			return response, nil
		}
		candidate, err := workerupdate.PreparedCandidateForRelease(release)
		if err != nil {
			return response, err
		}
		if request.Operation == "download" {
			journal, stageErr := stageWindowsActivation(ctx, c.config, release)
			if stageErr != nil {
				return response, stageErr
			}
			response.Candidate = &candidate
			response.Transaction = windowsTransactionState(journal)
			return response, nil
		}
		journal, err := loadWindowsActivationJournal(c.config)
		if err != nil {
			return response, errors.Join(workerupdate.ErrPreparedCandidate, err)
		}
		if journal.Stage != windowsActivationAwaitingApproval {
			return response, ErrApprovalRequired
		}
		if request.ApprovalID != candidate.ID || journal.Candidate.ID != candidate.ID {
			return response, ErrCandidateChanged
		}
		if err = verifyWindowsPreparedCandidate(ctx, journal); err != nil {
			return response, err
		}
		backend := newWindowsSCMActivationBackend(c.config)
		if release.SupervisorMaintenance {
			owner, ownerErr := backend.ownerControlClient()
			if ownerErr != nil {
				return response, ownerErr
			}
			if err := planOwnerMaintenance(ctx, c.config.StateRoot, owner, release, !automatic); err != nil {
				return response, err
			}
		}
		if err = backend.AuthorizeRecovery(ctx, journal); err != nil {
			return response, err
		}
		journal.ApprovedCandidateID = candidate.ID
		journal.ManualApproval = !automatic
		if err = backend.WriteJournal(journal); err != nil {
			return response, err
		}
		if err = installWindowsActivatorService(journal.Updater.Path, c.config.OwnerSID); err != nil {
			return response, err
		}
		journal.Stage = windowsActivationStaged
		if err = backend.WriteJournal(journal); err != nil {
			return response, err
		}
		response.Candidate = &candidate
		response.Version = release.Version
		response.Pending = true
		response.Transaction = windowsTransactionState(journal)
		return response, nil
	default:
		return ControlResponse{}, ErrInvalidControl
	}
}

func windowsTransactionState(journal windowsActivationJournal) workerupdate.TransactionState {
	state := workerupdate.TransactionState{
		Schema: workerupdate.TransactionSchemaV1, TransactionID: journal.TransactionID,
		ActiveVersion: journal.PreviousVersion, CandidateVersion: journal.Version,
		UpdatedAt: time.Now().UTC(),
	}
	switch journal.Stage {
	case windowsActivationAwaitingApproval:
		state.Stage = updateflow.StageAwaitingApproval
	case windowsActivationStaged:
		state.Stage = updateflow.StageStaged
	case windowsActivationSwitching:
		state.Stage = updateflow.StageCutover
	case windowsActivationServicesLive, windowsActivationCommitReady:
		state.Stage = updateflow.StageMonitoring
	case windowsActivationCommitted:
		state.Stage, state.ActiveVersion = updateflow.StageCommitted, journal.Version
	case windowsActivationRollingBack, windowsActivationRollbackReady:
		state.Stage = updateflow.StageRollback
	case windowsActivationRolledBack:
		state.Stage, state.Quarantined = updateflow.StageIdle, true
	}
	if journal.Failure != "" {
		state.Failure = updateflow.FailureHealth
	}
	return state
}

func activationRequested(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func (c *windowsController) checkRelease(ctx context.Context) (autoupdate.Result, error) {
	c.checkMu.Lock()
	defer c.checkMu.Unlock()
	result, err := resolveRelease(ctx, c.activeVersion, c.resolve)
	return autoupdate.Result{Version: result.Version}, err
}

func (c *windowsController) activationBlocked() (bool, error) {
	return c.activationBlockedContext(context.Background())
}

// activationBlockedReadOnlyContext reports whether the active version is
// fenced by a nonterminal transaction without attempting to hand the journal
// back to the one-shot activator. Status and check requests use this path so
// diagnostics cannot mutate SCM, binaries, or the journal.
func (c *windowsController) activationBlockedReadOnlyContext(_ context.Context) (bool, error) {
	if c == nil {
		return true, ErrWindowsActivationUnavailable
	}
	journal, err := loadWindowsActivationJournalForController(c.config)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	return windowsActivationBlocksVersion(journal, c.activeVersion), nil
}

// activationBlockedContext also repairs a live updater that started while an
// activator owned a transaction. Startup can observe that owner and continue,
// but the owner may then exit before publishing a terminal journal (for
// example after a failed rollback verification). In that case no later
// service start occurs, so the controller must reclaim any nonterminal journal
// that the activator can safely resume.
//
// The recovery helper repeats the SCM owner check immediately before creating
// or starting the activator service. activationMu only serializes requests in
// this controller; the SCM check remains the cross-process ownership guard.
func (c *windowsController) activationBlockedContext(ctx context.Context) (bool, error) {
	if c == nil {
		return true, ErrWindowsActivationUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.activationMu.Lock()
	defer c.activationMu.Unlock()

	journal, err := loadWindowsActivationJournalForController(c.config)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if !windowsActivationBlocksVersion(journal, c.activeVersion) {
		return false, nil
	}
	if windowsActivationNeedsControllerRecovery(journal, c.activeVersion) {
		if _, resumeErr := resumeWindowsActivationForController(ctx, c.config); resumeErr != nil {
			// A stale transaction must remain fenced if SCM recovery is
			// temporarily unavailable. Preserve the typed control error so
			// clients can retry instead of treating this as an unrelated check
			// failure.
			return true, errors.Join(ErrWindowsActivationUnavailable, resumeErr)
		}
		// The activator owns the transaction asynchronously. Re-read the
		// journal so a very fast rollback completion unblocks this request,
		// while a still-running recovery remains fenced.
		journal, err = loadWindowsActivationJournalForController(c.config)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return true, err
		}
	}
	return windowsActivationBlocksVersion(journal, c.activeVersion), nil
}

func controlErrorCodeWindows(err error) string {
	if errors.Is(err, workerupdate.ErrApprovalRequired) || errors.Is(err, ErrApprovalRequired) {
		return "approval_required"
	}
	if errors.Is(err, workerupdate.ErrPreparedCandidate) || errors.Is(err, ErrCandidateChanged) {
		return "candidate_changed"
	}
	var busy *autoupdate.ActiveTerminalSessionsError
	if errors.As(err, &busy) {
		return autoupdate.BlockedActiveTerminalSessions
	}
	if errors.Is(err, ErrWindowsActivationUnavailable) {
		return "activation_unavailable"
	}
	return "check_failed"
}

func compareWindowsInstalledVersion(candidate, active string, source installsource.Source) (int, error) {
	if source.Validate() == nil && source.Version == active && source.Distribution == installsource.Custom {
		return 1, nil
	}
	return selfupdate.CompareVersions(candidate, active)
}
