//go:build darwin || linux

// Package updated discovers updates, prepares verified artifacts for review, and
// installs only an exactly approved candidate, with native restart and recovery.
package updated

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseeligibility"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

var ErrInvalidConfig = errors.New("invalid paperboat-updated configuration")

// activationFailure retains the original error identity while exposing only a
// finite local diagnostic reason. Error text never formats the wrapped cause.
type activationFailure struct {
	phase, reason string
	cause         error
}

func (e *activationFailure) Error() string { return "update " + e.phase + " failed: " + e.reason }
func (e *activationFailure) Unwrap() error { return e.cause }

func healthFailure(reason string, cause error) error {
	return &activationFailure{phase: "health", reason: reason, cause: cause}
}

type Config struct {
	AutomaticUpdates     bool
	StateRoot            string
	Binary               string
	BinaryRollback       string
	BinaryStaged         string
	Active               workerupdate.Release
	WorkerUID            int
	WorkerGID            int
	SocketPath           string
	Token                []byte
	RepositoryURL        string
	MachineID            string
	Health               workerupdate.HealthChecker
	ActivationGate       workerupdate.ActivationGate
	Events               workerupdate.EventSink
	ActivationController UnixActivationController
	Participants         UnixParticipants
	Environment          map[string]string
	// RefreshManuals runs the committed executable's bundled-manual extractor.
	// Failure keeps the existing committed transaction pending for recovery.
	RefreshManuals     func(context.Context) error
	CommitInstallation func(context.Context, workerupdate.Release) error
	// ControlSocket is the fixed local socket exposed to the enrolled user for
	// pb update, check, and status. It is not an updater command channel.
	ControlSocket string
}

type Service struct {
	manager       *workerupdate.Manager
	source        workerupdate.TUFSource
	scheduler     *autoupdate.Scheduler
	control       controlServer
	controlMu     sync.Mutex
	managerMu     sync.RWMutex
	config        Config
	managerConfig workerupdate.Config
}

func New(config Config) (*Service, error) {
	if !filepath.IsAbs(config.StateRoot) || !filepath.IsAbs(config.ControlSocket) || !validUnixWorkerIdentity(config.WorkerUID, config.WorkerGID) || len(config.Token) != 32 || config.SocketPath == "" || config.RepositoryURL == "" || config.MachineID == "" || config.Health == nil || config.ActivationController == nil || config.Participants == nil {
		return nil, ErrInvalidConfig
	}
	if err := secureRoot(config.StateRoot); err != nil {
		return nil, err
	}
	tufRoot, err := secureChild(config.StateRoot, "tuf")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"index", "targets"} {
		if _, err := secureChild(tufRoot, name); err != nil {
			return nil, err
		}
	}
	client, err := hostdproto.NewClient(config.SocketPath, config.Token, 35*time.Second)
	if err != nil {
		return nil, err
	}
	if config.ActivationGate == nil {
		// Stability checks can span the signed policy's full 30-minute window.
		// The request context bounds the check; short control RPC deadlines must
		// not truncate it and roll back an otherwise healthy installation.
		gateClient, clientErr := hostdproto.NewClient(config.SocketPath, config.Token, 31*time.Minute)
		if clientErr != nil {
			return nil, clientErr
		}
		config.ActivationGate, err = workerupdate.NewDeploymentActivationGate(workerupdate.DeploymentActivationGateConfig{Provider: workerupdate.HostdDeploymentProvider{Client: gateClient}})
		if err != nil {
			return nil, err
		}
	}
	deferral, err := releaseeligibility.NewFileStore(filepath.Join(config.StateRoot, "deferral.json"))
	if err != nil {
		return nil, err
	}
	source := workerupdate.TUFSource{RepositoryURL: config.RepositoryURL, StateRoot: filepath.Join(config.StateRoot, "tuf"), MachineID: config.MachineID, FailureDomain: workerupdate.HostdFailureDomainSource{Client: client, MachineID: config.MachineID}, Deferral: deferral}
	service := &Service{source: source, config: config, managerConfig: workerupdate.Config{StatePath: filepath.Join(config.StateRoot, "transaction.json"), Binary: config.Binary, BinaryRollback: config.BinaryRollback, BinaryStaged: config.BinaryStaged, Active: config.Active, OwnerUID: 0, OwnerGID: 0, WorkerUID: config.WorkerUID, WorkerGID: config.WorkerGID, HostdEndpoint: config.SocketPath, Capability: config.Token, Fetcher: source, Hostd: client, Health: config.Health, Gate: config.ActivationGate, Events: config.Events, MonitorWindow: 10 * time.Minute, HealthInterval: time.Second}}
	service.managerConfig.ActivateRuntime = service.activateRuntime
	service.managerConfig.Starter = workerupdate.OwnerStarter{Client: client}
	service.managerConfig.AuthorizeOwnerMaintenance = func(ctx context.Context, release workerupdate.Release, manual bool) error {
		return authorizeOwnerMaintenance(ctx, config.StateRoot, client, release, manual)
	}
	service.managerConfig.AbortOwnerMaintenance = client.AbortMaintenance
	service.managerConfig.CommitRuntime = func(ctx context.Context, release workerupdate.Release) error {
		if err := verifyUnixExecutable(service.config.Binary, release); err != nil {
			return err
		}
		if service.config.CommitInstallation != nil {
			if err := service.config.CommitInstallation(ctx, release); err != nil {
				return err
			}
		}
		if service.config.RefreshManuals != nil {
			if err := service.config.RefreshManuals(ctx); err != nil {
				return err
			}
		}
		return autoupdate.ClearOwnerMaintenance(filepath.Join(config.StateRoot, "owner-maintenance.json"))
	}
	manager, err := service.newManager(config.Active)
	if err != nil {
		return nil, err
	}
	service.manager = manager
	scheduler, err := autoupdate.New(autoupdate.Config{Check: service.automaticCheck,
		NextCheck: func(now, next time.Time) time.Time {
			return nextMachineUpdateCheck(config.StateRoot, config.AutomaticUpdates, now, next)
		},
	})
	if err != nil {
		return nil, err
	}
	if err = seedUnixBlockedUpdate(config.StateRoot, scheduler); err != nil {
		return nil, err
	}
	service.scheduler = scheduler
	service.control = controlServer{socketPath: config.ControlSocket, uid: config.WorkerUID, gid: config.WorkerGID, invokeRequest: service.controlRequestWithRequest}
	return service, nil
}

func validUnixWorkerIdentity(uid, gid int) bool {
	return uid > 0 && gid > 0 || uid == 0 && gid == 0
}

func (s *Service) activateRuntime(ctx context.Context, version string) (hostdproto.Status, error) {
	if err := s.config.ActivationController.RestartHostd(ctx); err != nil {
		return hostdproto.Status{}, err
	}
	wantWorker := "runtime-" + strings.ReplaceAll(version, " ", "-")
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		status, err := s.managerConfig.Hostd.Active(ctx)
		if err == nil && status.State == hostdproto.StateActive && status.WorkerID == wantWorker && status.Epoch > 0 && status.LastHeartbeatUnixMilli >= time.Now().Add(-15*time.Second).UnixMilli() {
			handoff, handoffErr := readUnixHandoff(s.config.StateRoot)
			if handoffErr != nil {
				return hostdproto.Status{}, handoffErr
			}
			if handoff != nil {
				participants := &unixParticipantGate{participants: s.config.Participants, controller: s.config.ActivationController, handoff: handoff, persist: func() error { return writeUnixHandoff(s.config.StateRoot, handoff) }}
				if err := participants.restart(ctx, version); err != nil {
					return hostdproto.Status{}, err
				}
			}
			return status, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = workerupdate.ErrInvalidRelease
		}
		select {
		case <-ctx.Done():
			return hostdproto.Status{}, errors.Join(lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func seedUnixBlockedUpdate(root string, scheduler *autoupdate.Scheduler) error {
	var requiredVersion string
	var nextCheckAt time.Time
	handoff, err := readUnixHandoff(root)
	if err != nil {
		return err
	}
	if handoff != nil && handoff.ActiveTerminalBusy {
		requiredVersion, nextCheckAt = handoff.BusyRequiredVersion, handoff.BusyNextCheckAt
	}
	journal, err := updateflow.Load(filepath.Join(root, "transaction.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && journal.BlockedReason == autoupdate.BlockedActiveTerminalSessions {
		requiredVersion, nextCheckAt = journal.RequiredVersion, journal.NextCheckAt
	}
	if requiredVersion == "" {
		return nil
	}
	return scheduler.SeedBlockedActiveTerminalSessions(requiredVersion, nextCheckAt)
}

func (s *Service) Run(ctx context.Context) error {
	return s.run(ctx, nil)
}

// RunWithReady is the service-manager entry point for Unix service
// declarations using Type=notify. The callback runs only after the updater's
// authenticated control listener has been created, so READY=1 cannot race
// service initialization.
func (s *Service) RunWithReady(ctx context.Context, ready func() error) error {
	return s.run(ctx, ready)
}

func (s *Service) run(ctx context.Context, ready func() error) error {
	if s == nil || s.currentManager() == nil || s.scheduler == nil {
		return ErrInvalidConfig
	}
	if lock, lockErr := unixActivationLock(s.config.StateRoot); lockErr == nil {
		if pending, err := nativeInstallPending(s.config.StateRoot); err != nil {
			lock.Close()
			return err
		} else if pending {
			lock.Close()
			goto serveControl
		}
		handoff, handoffErr := readUnixHandoff(s.config.StateRoot)
		if handoffErr != nil {
			lock.Close()
			return handoffErr
		}
		if handoff == nil {
			// New already bound this manager to the verified executing release.
			// Recover owns reconciliation with an older native-install journal;
			// refreshing from that journal first would reject the new executable.
			if err := s.currentManager().Recover(ctx); err != nil {
				lock.Close()
				return err
			}
		} else if s.config.Active.Version != handoff.Previous.Version {
			// A separately verified native package install is allowed to supersede an
			// older interrupted transaction. The manager validates the signed active
			// identity and atomically resets only a permitted older journal before the
			// activation owner retires its now-obsolete helper and marker.
			if err := s.currentManager().Recover(ctx); err != nil {
				lock.Close()
				return err
			}
			state, err := s.currentManager().TransactionState()
			if err != nil || state.Stage != "idle" || state.ActiveVersion != s.config.Active.Version {
				lock.Close()
				return errors.Join(err, workerupdate.ErrBlocked)
			}
			if err := retireUnixHandoff(ctx, s.config.StateRoot, s.config.ActivationController); err != nil {
				lock.Close()
				return err
			}
		} else {
			state, err := s.currentManager().TransactionState()
			if err != nil {
				lock.Close()
				return err
			}
			if handoff.ActiveTerminalBusy && state.Stage == updateflow.StageIdle {
				if err = retireUnixHandoff(ctx, s.config.StateRoot, s.config.ActivationController); err != nil {
					lock.Close()
					return err
				}
			} else if !s.config.AutomaticUpdates && !handoff.Manual && !handoff.Started {
				if err = retireUnixHandoff(ctx, s.config.StateRoot, s.config.ActivationController); err != nil {
					lock.Close()
					return err
				}
			} else if err = s.config.ActivationController.Install(ctx, filepath.Join(s.config.StateRoot, "activation", "pb"), s.config.Environment); err != nil {
				lock.Close()
				return err
			}
		}
		lock.Close()
	} else if !errors.Is(lockErr, ErrActivationPending) {
		return lockErr
	}
serveControl:
	listener, err := s.control.listen()
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(s.control.socketPath)
	}()
	go s.control.serve(ctx, listener)
	if ready != nil {
		if err := ready(); err != nil {
			return err
		}
	}
	return s.scheduler.Run(ctx)
}

// Download prepares the signed candidate without launching it or stopping services.
func (s *Service) Download(ctx context.Context) (workerupdate.PreparedCandidate, error) {
	return s.downloadWithResolver(ctx, s.source.ResolveManual)
}

func (s *Service) downloadWithResolver(ctx context.Context, resolve workerupdate.Resolver) (workerupdate.PreparedCandidate, error) {
	if s == nil || s.currentManager() == nil {
		return workerupdate.PreparedCandidate{}, ErrInvalidConfig
	}
	lock, err := unixActivationLock(s.config.StateRoot)
	if err != nil {
		return workerupdate.PreparedCandidate{}, err
	}
	defer lock.Close()
	if handoff, err := readUnixHandoff(s.config.StateRoot); err != nil || handoff != nil {
		return workerupdate.PreparedCandidate{}, errors.Join(ErrActivationPending, err)
	}
	if err = s.refreshManager(); err != nil {
		return workerupdate.PreparedCandidate{}, err
	}
	release, found, err := resolve(ctx)
	if err != nil || !found {
		return workerupdate.PreparedCandidate{}, err
	}
	return s.currentManager().Prepare(ctx, release)
}

// Install persists exact local approval before handing installation to recovery.
func (s *Service) Install(ctx context.Context, approvalID string) (workerupdate.Result, error) {
	if !approvalIDPattern.MatchString(approvalID) {
		return workerupdate.Result{}, ErrInvalidControl
	}
	return s.queueActivation(ctx, approvalID)
}

func (s *Service) Snapshot() autoupdate.Observation {
	if s == nil || s.scheduler == nil {
		return autoupdate.Observation{}
	}
	return s.scheduler.Snapshot()
}

// Check resolves the signed cohort-eligible release but does not stage or
// activate it. Manual installation is deliberately a distinct control action.
func (s *Service) Check(ctx context.Context) (workerupdate.Result, error) {
	if s == nil || s.currentManager() == nil {
		return workerupdate.Result{}, ErrInvalidConfig
	}
	lock, err := unixActivationLock(s.config.StateRoot)
	if err != nil {
		return workerupdate.Result{Version: s.currentManager().ActiveVersion()}, err
	}
	defer lock.Close()
	if handoff, err := readUnixHandoff(s.config.StateRoot); err != nil {
		return workerupdate.Result{}, err
	} else if handoff != nil {
		return workerupdate.Result{Version: handoff.Candidate}, nil
	}
	if err := s.refreshManager(); err != nil {
		return workerupdate.Result{}, err
	}
	// A check only resolves and verifies the signed release metadata. It must
	// never call Manager.Check, which performs the full activation transaction
	// (including the health-monitoring hold) and made `pb update check` appear
	// hung while also unexpectedly installing an update.
	result, err := s.scheduler.ObserveCheck(ctx, func(ctx context.Context) (autoupdate.Result, error) {
		workerResult, resolveErr := resolveRelease(ctx, s.currentManager().ActiveVersion(), s.source.Resolve)
		return autoupdate.Result{Version: workerResult.Version}, resolveErr
	})
	return workerupdate.Result{Version: result.Version, Updated: false}, err
}

// HTTPHealth is a bounded local hostd readiness check. The endpoint must be a
// fixed loopback URL supplied by the service definition, not a release index
// or user-controlled redirect. It is evaluated on every hostd heartbeat during
// the monitoring hold, so a merely fenced but unhealthy worker cannot commit.
type HTTPHealth struct {
	Endpoint string
	Client   *http.Client
}

func (h HTTPHealth) Check(ctx context.Context, status hostdproto.Status, _ workerupdate.Release) error {
	if status.State != hostdproto.StateActive || status.WorkerID == "" || status.Epoch == 0 {
		return healthFailure("owner_identity", ErrInvalidConfig)
	}
	if status.LastHeartbeatUnixMilli == 0 {
		return healthFailure("heartbeat_missing", ErrInvalidConfig)
	}
	if time.Since(time.UnixMilli(status.LastHeartbeatUnixMilli)) > 15*time.Second {
		return healthFailure("heartbeat_stale", ErrInvalidConfig)
	}
	parsed, err := url.Parse(h.Endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/healthz" || net.ParseIP(parsed.Hostname()) == nil || !net.ParseIP(parsed.Hostname()).IsLoopback() {
		return healthFailure("endpoint_invalid", ErrInvalidConfig)
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Endpoint, nil)
	if err != nil {
		return healthFailure("request_invalid", err)
	}
	response, err := client.Do(req)
	if err != nil {
		return healthFailure("transport", err)
	}
	defer response.Body.Close()
	var body struct {
		Live bool `json:"live"`
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return healthFailure("http_redirect", ErrInvalidConfig)
	}
	if response.StatusCode != http.StatusOK {
		return healthFailure("http_status", ErrInvalidConfig)
	}
	if json.NewDecoder(io.LimitReader(response.Body, 8<<10)).Decode(&body) != nil {
		return healthFailure("http_body", ErrInvalidConfig)
	}
	if !body.Live {
		return healthFailure("http_not_live", ErrInvalidConfig)
	}
	return nil
}

func secureRoot(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return ErrInvalidConfig
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != 0 {
		return ErrInvalidConfig
	}
	return nil
}

// secureChild provisions only a fixed component below an already validated
// root-owned directory. It never follows caller-selected paths or repairs an
// unsafe existing entry.
func secureChild(parent, name string) (string, error) {
	if name == "" || filepath.Base(name) != name {
		return "", ErrInvalidConfig
	}
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := secureRoot(path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Service) newManager(active workerupdate.Release) (*workerupdate.Manager, error) {
	return s.newManagerWithGate(active, s.config.ActivationGate, false)
}
func (s *Service) newManagerWithGate(active workerupdate.Release, gate workerupdate.ActivationGate, manual bool) (*workerupdate.Manager, error) {
	config := s.managerConfig
	config.Active = active
	config.Gate = gate
	config.ManualActivation = manual
	return workerupdate.New(config)
}
