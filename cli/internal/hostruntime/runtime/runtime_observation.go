//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/backoff"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/availability"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type runtimeObservationService struct {
	mu       sync.Mutex
	sender   *runtimeObservationSender
	interval time.Duration
	timeout  time.Duration
	// servicesWatch is how often local listening ports are compared with the last announcement;
	// zero uses machineServicesWatchInterval.
	servicesWatch time.Duration
	cancel        context.CancelFunc
	done          chan struct{}
}

// machineServicesWatchInterval bounds how long a newly listening port waits before it is announced:
// the periodic observation alone would make other computers wait for the next one.
const machineServicesWatchInterval = 2 * time.Second

func (s *runtimeObservationService) Start(ctx context.Context) error {
	if s.sender == nil || s.interval <= 0 || s.timeout <= 0 {
		return ErrProductionInvalid
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	initial, cancel := context.WithTimeout(ctx, s.timeout)
	err := s.sender.Send(initial)
	cancel()
	if err != nil {
		// Presence is the required part of this service. A first observation
		// can fail while credentials, DNS, or an auxiliary local store is
		// recovering. Do not let one transient failure make the optional
		// component disappear permanently: install the stable loop and let its
		// bounded sends retry on the normal heartbeat cadence.
		reportRuntimeObservationAttempt(initial, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	// Service.Start's context bounds startup only. The stable daemon owns the
	// accepted service until it calls Shutdown; tying the loop to the caller's
	// startup context silently stops machine presence after successful startup
	// on supervisors that cancel that context. Partial starts are still safe
	// because hostd always invokes Shutdown for an accepted or failed component.
	runCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done = stop, make(chan struct{})
	go s.loop(runCtx, s.done)
	return nil
}

// runtimeObservationMaxRetry caps the wait between observations while the server keeps failing them.
const runtimeObservationMaxRetry = time.Minute

// runtimeObservationReconnectRetry is the wait after an observation whose connection could not even
// be opened (DNS, route or connect failed): the server marks a silent machine offline after
// OfflineAfter and refuses descriptors for it, so presence is the first thing a machine whose network
// came back must report. Such a retry never reaches the server.
const runtimeObservationReconnectRetry = 2 * time.Second

// serverAnswered reports whether a failed observation reached the server and got a failing answer.
// Only those back off: failures that never reached the server cost it nothing.
func serverAnswered(err error) bool {
	var failure runtimeObservationFailure
	return errors.As(err, &failure) && failure.answered
}

// connectionNotOpened reports whether an observation failed before any connection to the server
// existed. A timeout on an open connection is not one: that server may be hung, and stays at the
// interval.
func connectionNotOpened(err error) bool {
	var dns *net.DNSError
	var network *net.OpError
	return errors.As(err, &dns) || errors.As(err, &network) && network.Op == "dial"
}

// runtimeObservationWait is the wait before the next observation after err (nil on success) and
// failures consecutive answered failures.
func (s *runtimeObservationService) runtimeObservationWait(err error, failures int) time.Duration {
	switch {
	case failures > 1:
		return backoff.Retry(failures-1, s.interval, runtimeObservationMaxRetry)
	case err != nil && !serverAnswered(err) && connectionNotOpened(err):
		return backoff.Jitter(runtimeObservationReconnectRetry)
	}
	return backoff.Jitter(s.interval)
}

func (s *runtimeObservationService) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	// The next observation follows the interval with jitter, so a fleet that reconnected together (a
	// server deploy) does not report in lockstep; consecutive failures the server answered back off to
	// runtimeObservationMaxRetry instead of every machine asking a failing server every interval.
	next := time.NewTimer(backoff.Jitter(s.interval))
	defer next.Stop()
	failures := 0
	watchEvery := s.servicesWatch
	if watchEvery <= 0 {
		watchEvery = machineServicesWatchInterval
	}
	watch := time.NewTicker(watchEvery)
	defer watch.Stop()
	send := func() error {
		sendCtx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		err := s.sender.Send(sendCtx)
		if err != nil {
			reportRuntimeObservationAttempt(sendCtx, err)
		}
		if serverAnswered(err) {
			failures++
		} else {
			failures = 0
		}
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-next.C:
			err := send()
			next.Reset(s.runtimeObservationWait(err, failures))
		case <-watch.C:
			if s.sender.machineServicesChanged(ctx) {
				send()
			}
		}
	}
}

func (s *runtimeObservationService) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	final, stop := context.WithTimeout(ctx, s.timeout)
	err := s.sender.Send(final)
	stop()
	return err
}

type runtimeObservationSender struct {
	machineServicesDiscover                             func(context.Context) ([]machineservices.Service, error)
	machineServicesMu                                   sync.Mutex
	machineServicesFaultMu                              sync.Mutex
	machineServicesFaults                               [2]errorreport.Fault
	machineServicesPolicy                               clientapi.MachineServicesPolicy
	machineServicesPorts                                []clientapi.MachineServicePort
	machineServicesGeneration                           uint64
	endpoint, environmentID, machineID, reporterVersion string
	tokens                                              interface {
		Token(context.Context) (string, error)
	}
	proofs interface {
		Proof(context.Context, string, string, string, []byte) ([]byte, error)
	}
	operationID  func() (string, error)
	client       *http.Client
	availability interface {
		Observation() *availability.Observation
	}
	updater interface {
		Status(context.Context) (updated.ControlResponse, error)
	}
	layers                 interface{ FlushLayerObservations(context.Context) error }
	receiptPath            string
	installationGeneration uint64
	workerGeneration       uint64
	osBootID               string
	lazyBootID             string
	lazyStartedAt          time.Time
	serviceScope           string
	connector              interface{ Status() connector.Status }
	transferPolicy         *filetransfer.PolicyStore
	capabilities           []string
	capabilitiesController *machineCapabilityController
	environmentFaultMu     sync.Mutex
	environmentFaults      [4]errorreport.Fault
}

func (s *runtimeObservationSender) Send(ctx context.Context) error {
	return s.send(ctx, true)
}

func (s *runtimeObservationSender) send(ctx context.Context, mayConfirm bool) error {
	if s.layers != nil {
		s.observeEnvironmentFailure(ctx, 0, s.layers.FlushLayerObservations(ctx))
	}
	now := time.Now().UTC()
	observedCapabilities := s.capabilitiesController.Observation(now)
	availabilityState := availabilityObservation(s.availability)
	var updaterState *updated.ControlResponse
	var updaterErr error
	if s.updater != nil {
		status, err := s.updater.Status(ctx)
		if err != nil {
			updaterErr = err
		} else {
			updaterState = &status
		}
	}
	body, err := json.Marshal(struct {
		MachineServices     *clientapi.MachineServicesSnapshot `json:"machine_services,omitempty"`
		LazyRuntime         *lazyRuntimeObservation            `json:"lazy_runtime,omitempty"`
		EnvironmentID       string                             `json:"environment_id"`
		ResourceID          string                             `json:"resource_id"`
		ReporterVersion     string                             `json:"reporter_version"`
		SampledAt           time.Time                          `json:"sampled_at"`
		Availability        *availability.Observation          `json:"availability,omitempty"`
		RuntimeDiagnostics  *runtimeDiagnosticsObservation     `json:"runtime_diagnostics,omitempty"`
		Update              *runtimeUpdateObservation          `json:"update,omitempty"`
		MachineCapabilities *machineCapabilitiesObservation    `json:"machine_capabilities,omitempty"`
	}{
		MachineServices:     s.machineServicesObservation(ctx),
		LazyRuntime:         s.lazyRuntimeObservation(),
		EnvironmentID:       s.environmentID,
		ResourceID:          s.machineID,
		ReporterVersion:     s.reporterVersion,
		SampledAt:           now,
		Availability:        availabilityState,
		RuntimeDiagnostics:  s.runtimeDiagnostics(now, s.layers != nil),
		Update:              s.updateObservationFrom(now, availabilityState, updaterState, updaterErr),
		MachineCapabilities: observedCapabilities,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	token, err := s.tokens.Token(ctx)
	if err != nil {
		return runtimeObservationFailure{stage: "peer_authority", cause: err}
	}
	operationID, err := s.operationID()
	if err != nil {
		return err
	}
	proof, err := s.proofs.Proof(ctx, operationID, http.MethodPost, "/v1/runtime-observations", body)
	if err != nil {
		return runtimeObservationFailure{stage: "peer_authority", cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return runtimeObservationFailure{stage: "control_request", cause: err}
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	defer func() {
		for index := range responseBody {
			responseBody[index] = 0
		}
	}()
	if readErr != nil {
		return runtimeObservationFailure{stage: "delivery", cause: readErr}
	}
	if len(responseBody) > 8<<20 {
		return errors.New("runtime observation response is invalid")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.applyMachineServicesPolicy(nil)
		return runtimeObservationFailure{stage: "control_request", cause: errorreport.HTTPStatusFailure(response), answered: true}
	}
	s.applyMachineServicesPolicy(responseBody)
	if s.transferPolicy != nil {
		if err := applyRuntimeTransferPolicy(responseBody, s.transferPolicy); err != nil {
			return err
		}
	}
	if s.capabilitiesController != nil {
		acknowledged, err := applyRuntimeMachineCapabilities(ctx, responseBody, s.capabilitiesController)
		if err != nil {
			return err
		}
		applied := s.capabilitiesController.Observation(now)
		if observedCapabilities == nil || applied.Version != observedCapabilities.Version || applied.Applied != observedCapabilities.Applied || applied.Status != observedCapabilities.Status {
			// Presence accepts the policy fetch before the local reconciliation.
			// Confirm the applied revision before publishing the readiness receipt.
			if !mayConfirm {
				return errors.New("machine capability policy changed during confirmation")
			}
			return s.send(ctx, false)
		}
		if !acknowledged {
			return errors.New("server has not accepted applied machine capabilities")
		}
	}
	if s.receiptPath == "" {
		return nil
	}
	return writeServerHeartbeatReceipt(s.receiptPath, serverHeartbeatReceipt{Schema: "paperboat.server-heartbeat/v1", WorkerGeneration: s.workerGeneration, ReporterVersion: s.reporterVersion, AcceptedAt: time.Now().UTC()})
}

type runtimeObservationFailure struct {
	stage string
	cause error
	// answered is set when the server returned a failing status.
	answered bool
}

func (runtimeObservationFailure) Error() string                   { return "runtime observation failed" }
func (failure runtimeObservationFailure) Unwrap() error           { return failure.cause }
func (failure runtimeObservationFailure) DiagnosticStage() string { return failure.stage }
func (runtimeObservationFailure) DiagnosticCode() string          { return "control_request_failed" }

func reportRuntimeObservationAttempt(ctx context.Context, err error) {
	if errorreport.HTTPAttemptObserved(err) {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "runtime_observation", "reconciliation", "control_request_failed", err)
}

// Four producer-owned phases bound the retained retry classifications. Only a
// changed fault or actual successful recovery produces another local event.
func (s *runtimeObservationSender) observeEnvironmentFailure(ctx context.Context, phase int, err error) {
	stage := "reconciliation"
	if phase < 2 {
		stage = "diagnostic_storage"
	}
	fault := errorreport.ProjectFault(ctx, "paperboat-daemon", "config_sync", stage, "environment_sync_failed", err)
	if fault.Outcome == "canceled" {
		return
	}
	s.environmentFaultMu.Lock()
	previous := s.environmentFaults[phase]
	s.environmentFaults[phase] = fault
	s.environmentFaultMu.Unlock()
	if err == nil {
		if previous.Code != "" {
			errorreport.Current().Lifecycle(ctx, "config", "config_sync", "recovered", "success")
			if local := diagnostics.FromContext(ctx); local != nil {
				_ = local.RecordWithSupportReference(stage, "recovered", "info", supportref.FromContext(ctx), map[string]string{"component": "paperboat-daemon", "operation": "config_sync"})
			}
		}
		return
	}
	if previous.Code == fault.Code && previous.Stage == fault.Stage && previous.Cause == fault.Cause && previous.Errno == fault.Errno && previous.HTTPStatus == fault.HTTPStatus {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "config_sync", stage, "environment_sync_failed", err)
}

type lazyRuntimeObservation struct {
	Schema                 string    `json:"schema"`
	BootID                 string    `json:"boot_id"`
	InstallationGeneration uint64    `json:"installation_generation"`
	StartedAt              time.Time `json:"started_at"`
}

func (s *runtimeObservationSender) lazyRuntimeObservation() *lazyRuntimeObservation {
	if s.lazyBootID == "" || s.installationGeneration == 0 || s.lazyStartedAt.IsZero() {
		return nil
	}
	return &lazyRuntimeObservation{Schema: "paperboat.lazy-runtime/v1", BootID: s.lazyBootID, InstallationGeneration: s.installationGeneration, StartedAt: s.lazyStartedAt.UTC()}
}

type runtimeUpdateObservation struct {
	Schema                 string    `json:"schema"`
	State                  string    `json:"state"`
	CurrentVersion         string    `json:"current_version"`
	TargetVersion          string    `json:"target_version,omitempty"`
	Channel                string    `json:"channel"`
	OperationID            string    `json:"operation_id"`
	InstallationGeneration uint64    `json:"installation_generation"`
	WorkerGeneration       uint64    `json:"worker_generation"`
	OSBootID               string    `json:"os_boot_id"`
	RollbackCount          uint64    `json:"rollback_count"`
	ErrorCode              string    `json:"error_code,omitempty"`
	ObservedAt             time.Time `json:"observed_at"`
}

func (s *runtimeObservationSender) updateObservation(now time.Time, availabilityState *availability.Observation) *runtimeUpdateObservation {
	return s.updateObservationFrom(now, availabilityState, nil, nil)
}

func (s *runtimeObservationSender) updateObservationFrom(now time.Time, availabilityState *availability.Observation, updaterState *updated.ControlResponse, updaterErr error) *runtimeUpdateObservation {
	if s.reporterVersion == "" || s.installationGeneration == 0 || s.workerGeneration == 0 || s.osBootID == "" {
		return nil
	}
	state, target, errorCode := "healthy", "", ""
	channel := "custom"
	if buildinfo.Distribution == "official" {
		channel = "stable"
	}
	var rollbackCount uint64
	if updaterErr != nil {
		return &runtimeUpdateObservation{
			Schema: "paperboat.update-observation/v1", State: "failed", CurrentVersion: s.reporterVersion,
			TargetVersion: s.reporterVersion, Channel: channel, OperationID: "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
			InstallationGeneration: s.installationGeneration, WorkerGeneration: s.workerGeneration, OSBootID: s.osBootID,
			ErrorCode: "updater_unavailable", ObservedAt: now,
		}
	}
	if updaterState != nil {
		currentVersion := s.reporterVersion
		if updaterState.Version != "" {
			currentVersion = updaterState.Version
		}
		if updaterState.Observation.Failure != "" || updaterState.Status != "ok" {
			return &runtimeUpdateObservation{
				Schema: "paperboat.update-observation/v1", State: "failed", CurrentVersion: currentVersion,
				TargetVersion: currentVersion, Channel: channel, OperationID: "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
				InstallationGeneration: s.installationGeneration, WorkerGeneration: s.workerGeneration, OSBootID: s.osBootID,
				ErrorCode: "update_failed", ObservedAt: now,
			}
		}
		// The updater owns activation and is authoritative for the installed
		// version. The runtime process can remain alive across that activation.
		return &runtimeUpdateObservation{
			Schema: "paperboat.update-observation/v1", State: "healthy", CurrentVersion: currentVersion,
			Channel: channel, OperationID: "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
			InstallationGeneration: s.installationGeneration, WorkerGeneration: s.workerGeneration, OSBootID: s.osBootID,
			ObservedAt: now,
		}
	}
	if availabilityState != nil {
		if availabilityState.UpdateHealth == "unknown" {
			return nil
		}
		rollbackCount = availabilityState.UpdateRollbacks
		if availabilityState.UpdateHealth == "recovery_required" {
			state, target, errorCode = "failed", s.reporterVersion, "recovery_required"
		}
	}
	return &runtimeUpdateObservation{
		Schema:                 "paperboat.update-observation/v1",
		State:                  state,
		CurrentVersion:         s.reporterVersion,
		TargetVersion:          target,
		Channel:                channel,
		OperationID:            "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
		InstallationGeneration: s.installationGeneration,
		WorkerGeneration:       s.workerGeneration,
		OSBootID:               s.osBootID,
		RollbackCount:          rollbackCount,
		ErrorCode:              errorCode,
		ObservedAt:             now,
	}
}

type runtimeDiagnosticsObservation struct {
	Capabilities        []string  `json:"capabilities"`
	WorkerGeneration    uint64    `json:"worker_generation"`
	OSBootID            string    `json:"os_boot_id"`
	ConnectorState      string    `json:"connector_state"`
	ConnectorGeneration uint64    `json:"connector_generation"`
	WorkerServiceScope  string    `json:"worker_service_scope"`
	ObservedAt          time.Time `json:"observed_at"`
}

func (s *runtimeObservationSender) runtimeDiagnostics(observedAt time.Time, environmentEnabled bool) *runtimeDiagnosticsObservation {
	if s.workerGeneration < 1 || s.osBootID == "" || s.connector == nil {
		return nil
	}
	status := s.connector.Status()
	state := "unavailable"
	if status.Connected {
		state = "ready"
	} else if status.Stopping {
		state = "degraded"
	}
	capabilities := append([]string(nil), s.capabilities...)
	if len(capabilities) == 0 {
		capabilities = []string{"file_receive", "preview_launch", "terminal_host", "session_host", "keep_awake"}
	}
	if environmentEnabled && !slices.Contains(capabilities, "environment_injection") {
		capabilities = append(capabilities, "environment_injection")
	}
	return &runtimeDiagnosticsObservation{Capabilities: capabilities, WorkerGeneration: s.workerGeneration, OSBootID: s.osBootID, ConnectorState: state, ConnectorGeneration: status.Generation, WorkerServiceScope: s.serviceScope, ObservedAt: observedAt}
}

type serverHeartbeatReceipt struct {
	Schema           string    `json:"schema"`
	WorkerGeneration uint64    `json:"worker_generation"`
	ReporterVersion  string    `json:"reporter_version"`
	AcceptedAt       time.Time `json:"accepted_at"`
}

func writeServerHeartbeatReceipt(path string, receipt serverHeartbeatReceipt) error {
	if !filepath.IsAbs(path) || receipt.WorkerGeneration < 1 || receipt.ReporterVersion == "" || receipt.AcceptedAt.IsZero() {
		return ErrProductionInvalid
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrProductionInvalid
	}
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}

func availabilityObservation(source interface {
	Observation() *availability.Observation
}) *availability.Observation {
	if source == nil {
		return nil
	}
	return source.Observation()
}
