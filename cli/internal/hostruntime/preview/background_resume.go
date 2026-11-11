package preview

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// BackgroundRestoreWindow bounds how long a restarted host runtime keeps trying
// to resume a background preview. It stays under the server's background owner
// grace, after which the server releases the preview anyway.
const BackgroundRestoreWindow = 3 * time.Minute

const (
	backgroundRestoreFirstDelay = time.Second
	backgroundRestoreMaxDelay   = 15 * time.Second
	backgroundDispatchValidity  = time.Minute

	foregroundRestoreWindow     = 30 * time.Second
	foregroundRestoreFirstDelay = 250 * time.Millisecond
	foregroundRestoreMaxDelay   = time.Second
)

// BackgroundResume lets background previews outlive a restart of the host
// runtime (an update, a crash, a manual restart). While the runtime runs it
// records each background preview; when it shuts down it leaves their leases
// running; the next start dispatches them again with the same preview and URL.
// A preview ends only on an explicit stop, its own deadline, or its origin
// staying gone. After a reboot nothing is resumed.
type BackgroundResume struct {
	Store *BackgroundStore
	// OSBootID identifies this boot of the machine.
	OSBootID string
	// IsBackground reports whether the local owner lease was transferred to the host runtime.
	IsBackground func(ownerSessionID string) bool
	// Current reads the server's present projection of a stored lease.
	Current func(ctx context.Context, stored Lease) (Lease, error)
}

type backgroundOwnerRegistry interface {
	RegisterMachineOwnerSession(machineID, ownerSessionID string, target LeaseTarget) (<-chan struct{}, string, error)
	ReleaseMachineOwnerSession(machineID, ownerSessionID string) error
	LocalOwnerAcquired(ownerSessionID string) bool
}

// resumeRecord says whether a local-lease dispatch is recorded so a restarted
// host runtime resumes it, and whether it is a foreground one. Background
// previews resume on their own; foreground ones once their local lease is
// held here: taken over from the previous generation at start
// (owner_lease_handover.go) or reacquired by the CLI (pbvm4 feat5: pb update
// ended both foreground units 30 s after the worker that renewed them went away).
func (m *DispatchManager) resumeRecord(request DispatchRequest, resumed *BackgroundRecord) (BackgroundRecord, bool) {
	resume := m.config.Background
	if resume == nil || resume.Store == nil || request.OwnerSessionKind != OwnerSessionLocalLease {
		return BackgroundRecord{}, false
	}
	record := BackgroundRecord{OSBootID: resume.OSBootID, Request: request, Foreground: true}
	if resumed != nil {
		record.Foreground = resumed.Foreground
	} else if request.UserDeadline != nil && resume.IsBackground != nil && resume.IsBackground(request.OwnerSessionID) {
		record.Foreground = false
	}
	return record, true
}

func (m *DispatchManager) rememberBackground(record BackgroundRecord) {
	if err := m.config.Background.Store.Save(record); err != nil {
		slog.Warn("preview will not survive a host runtime restart", "preview", record.Request.PreviewID, "error", err)
	}
}

func (m *DispatchManager) forgetBackground(operationID string) {
	if err := m.config.Background.Store.Remove(operationID); err != nil {
		slog.Warn("background preview record could not be removed", "operation", operationID, "error", err)
	}
}

// RestoreBackground resumes the background previews recorded by a previous run
// of this host runtime on the same boot. It blocks until every record has been
// resumed or dropped, so run it in the background of the runtime start.
func (m *DispatchManager) RestoreBackground(ctx context.Context) {
	if m == nil {
		return
	}
	resume := m.config.Background
	if resume == nil || resume.Store == nil {
		return
	}
	records, err := resume.Store.Load()
	if err != nil {
		slog.Warn("background previews could not be read", "error", err)
		return
	}
	for _, record := range records {
		if record.OSBootID != resume.OSBootID {
			m.forgetBackground(record.Request.OperationID)
			continue
		}
		if err := m.restoreOne(ctx, record); err != nil {
			slog.Warn("background preview was not resumed", "preview", record.Request.PreviewID, "error", err)
			m.forgetBackground(record.Request.OperationID)
		}
	}
}

func (m *DispatchManager) restoreOne(ctx context.Context, stored BackgroundRecord) error {
	window, delay, maxDelay := BackgroundRestoreWindow, backgroundRestoreFirstDelay, backgroundRestoreMaxDelay
	if stored.Foreground {
		// The server ends a foreground preview after its 30 s owner grace.
		window, delay, maxDelay = foregroundRestoreWindow, foregroundRestoreFirstDelay, foregroundRestoreMaxDelay
	}
	deadline := m.config.Now().Add(window)
	var last error
	for {
		last = m.restoreAttempt(ctx, stored)
		if last == nil || !errors.Is(last, ErrDispatchUnavailable) {
			return last
		}
		if !m.config.Now().Add(delay).Before(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay *= 2; delay > maxDelay {
			delay = maxDelay
		}
	}
}

// restoreAttempt dispatches the stored preview once. ErrDispatchUnavailable
// means "try again"; any other error means the preview cannot be resumed.
func (m *DispatchManager) restoreAttempt(ctx context.Context, record BackgroundRecord) error {
	resume := m.config.Background
	stored := record.Request
	registry, ok := m.config.Owners.(backgroundOwnerRegistry)
	if !ok {
		return ErrDispatchInvalid
	}
	if record.Foreground && !registry.LocalOwnerAcquired(stored.OwnerSessionID) {
		return ErrDispatchUnavailable
	}
	current, err := resume.Current(ctx, stored.Lease())
	if err != nil {
		return err
	}
	now := m.config.Now()
	if !validActiveLeaseState(current.State) || !current.LeaseDeadline.After(now) {
		return ErrLeaseExpired
	}
	if !record.Foreground && (current.UserDeadline == nil || !current.UserDeadline.After(now)) {
		return ErrLeaseExpired
	}
	request := refreshedDispatch(stored, current)
	hash, err := request.ComputeRequestHash()
	if err != nil {
		return err
	}
	request.RequestHash = hash
	authorization := DispatchAuthorization{
		AccountID: request.AccountID, ActorID: request.ActorID, MachineID: request.OwnerMachineID,
		OwnerSessionID: request.OwnerSessionID, OwnerSessionKind: request.OwnerSessionKind,
		PreviewID: request.PreviewID, OperationID: request.OperationID, ExpectedGeneration: request.ExpectedGeneration,
		IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID, CorrelationID: request.CorrelationID,
		RequestHash: hash, ExpiresAt: now.Add(backgroundDispatchValidity),
	}
	if !record.Foreground {
		if _, _, err := registry.RegisterMachineOwnerSession(request.OwnerMachineID, request.OwnerSessionID, request.Target); err != nil {
			return errors.Join(ErrDispatchInvalid, err)
		}
		// The dispatch takes its own owner reference; this one only bridges the registration.
		defer registry.ReleaseMachineOwnerSession(request.OwnerMachineID, request.OwnerSessionID)
	}
	_, err = m.dispatch(ctx, authorization, request, &record)
	return err
}

// refreshedDispatch carries the stored operation identity over the server's
// current lease so the dispatch is valid for the lease's present generation.
func refreshedDispatch(stored DispatchRequest, current Lease) DispatchRequest {
	request := stored
	request.Target.HostHeader = current.Target.HostHeader // the server lease is the authority for the Host the origin gets
	request.LeaseDeadline = current.LeaseDeadline.UTC()
	request.UserDeadline = utcTimePointer(current.UserDeadline)
	request.LeaseETag = current.ETag
	request.ExpectedGeneration = current.Generation
	request.State, request.AllocationState, request.EdgeState = current.State, current.AllocationState, current.EdgeState
	request.OriginState = current.OriginState
	if !validOriginState(request.OriginState) {
		request.OriginState = "unknown"
	}
	request.LastRenewedAt = current.LastRenewedAt.UTC()
	return request
}
