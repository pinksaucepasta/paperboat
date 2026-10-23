package browserterminal

import (
	"context"
	"math"
	"reflect"

	"github.com/pinksaucepasta/paperboat/server/internal/inspectaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

type MachineAuthorizer struct {
	Service   *usermachines.Service
	Inspector *inspectaccess.Service
}

func (a MachineAuthorizer) ResolveRole(ctx context.Context, userID, terminalSessionID string) (Decision, error) {
	if a.Service == nil {
		return Decision{}, ErrHostUnavailable
	}
	current, err := a.Service.ResolveTerminalBrowserRole(ctx, userID, terminalSessionID)
	if err != nil {
		return Decision{}, err
	}
	return machineBrowserDecision(current, err)
}

// CheckBandwidth refuses a terminal ticket while the machine's paying workspace is out of
// hosted allowance on the edge that would carry it.
func (a MachineAuthorizer) CheckBandwidth(ctx context.Context, decision Decision) error {
	if a.Service == nil {
		return ErrHostUnavailable
	}
	return a.Service.CheckBrowserTerminalBandwidth(ctx, decision.MachineID, decision.EdgeNodeID)
}

func (a MachineAuthorizer) CheckBrowserTerminalLimit(ctx context.Context, decision Decision) error {
	if a.Service == nil {
		return ErrHostUnavailable
	}
	return a.Service.CheckBrowserTerminalLimit(ctx, decision.MachineID, decision.TerminalSessionID)
}

func (a MachineAuthorizer) ResolveGuestRole(ctx context.Context, guestID, terminalSessionID string) (Decision, error) {
	if a.Service == nil {
		return Decision{}, ErrHostUnavailable
	}
	current, err := a.Service.ResolveTerminalGuestRole(ctx, guestID, terminalSessionID)
	return machineBrowserDecision(current, err)
}
func machineBrowserDecision(current usermachines.TerminalBrowserDecision, err error) (Decision, error) {
	if err != nil {
		return Decision{}, err
	}
	if current.PolicyGeneration > math.MaxInt64 || current.TeamGeneration > math.MaxInt64 || current.MembershipGeneration > math.MaxInt64 || current.BindingGeneration > math.MaxInt64 {
		return Decision{}, ErrHostUnavailable
	}
	subjectID := current.AccountID
	if current.GuestSessionID != "" {
		subjectID = current.GuestSessionID
	}
	return Decision{GuestSessionID: current.GuestSessionID, UserID: subjectID, OwnerAccountID: current.OwnerAccountID, WorkspaceID: current.WorkspaceID, TerminalSessionID: current.SessionID, MachineID: current.MachineID, EnvironmentID: current.EnvironmentID, HostWSSURL: current.HostWSSURL, EdgeNodeID: current.EdgeNodeID, EdgeProcessEpoch: current.EdgeProcessEpoch, EdgeEndpointHost: current.EdgeEndpointHost, Role: string(current.Role), PolicyGeneration: int64(current.PolicyGeneration), TeamGeneration: int64(current.TeamGeneration), MembershipGeneration: int64(current.MembershipGeneration), BindingGeneration: int64(current.BindingGeneration)}, nil
}

func (a MachineAuthorizer) SnapshotProcess(ctx context.Context, admission Decision) (int64, error) {
	if a.Service == nil {
		return 0, ErrHostUnavailable
	}
	// Resolve again so a forged or stale Decision cannot ask the host for a
	// different resource. The gateway checks this role after the snapshot too.
	var current usermachines.TerminalBrowserDecision
	var err error
	if admission.GuestSessionID != "" {
		current, err = a.Service.ResolveTerminalGuestRole(ctx, admission.GuestSessionID, admission.TerminalSessionID)
	} else {
		current, err = a.Service.ResolveTerminalBrowserRole(ctx, admission.UserID, admission.TerminalSessionID)
	}
	if err != nil {
		if expectedTerminalRejection(err) {
			return 0, ErrForbidden
		}
		return 0, unavailableCause(err)
	}
	resolved, err := machineBrowserDecision(current, nil)
	if err != nil || !sameDecision(admission, resolved) {
		return 0, ErrForbidden
	}

	snapshot, err := a.Service.SnapshotTerminalForBrowser(ctx, current)
	if err != nil {
		if expectedTerminalRejection(err) {
			return 0, ErrForbidden
		}
		return 0, unavailableCause(err)
	}
	if snapshot.Generation < 1 || snapshot.Generation > math.MaxInt64 {
		return 0, ErrHostUnavailable
	}
	return int64(snapshot.Generation), nil
}

type admissionFailure struct {
	classification error
	cause          error
}

func (e admissionFailure) Error() string { return e.classification.Error() }

func (e admissionFailure) Unwrap() []error {
	if e.cause == nil {
		return []error{e.classification}
	}
	return []error{e.classification, e.cause}
}

func unavailableCause(cause error) error {
	if cause == nil || cause == ErrHostUnavailable {
		return ErrHostUnavailable
	}
	return admissionFailure{classification: ErrHostUnavailable, cause: cause}
}

func expectedTerminalRejection(err error) bool {
	return allErrorLeavesMatch(err,
		ErrForbidden,
		ErrTicketInvalid,
		usermachines.ErrTerminalSessionNotFound,
		usermachines.ErrTerminalBrowserSessionClosed,
		usermachines.ErrBandwidthQuotaExhausted,
	)
}

func pureTerminalCancellation(err error) bool {
	return allErrorLeavesMatch(err, context.Canceled)
}

func allErrorLeavesMatch(err error, allowed ...error) bool {
	if err == nil {
		return false
	}
	nodes, leaves := 0, 0
	var visit func(error, int) bool
	visit = func(current error, depth int) bool {
		nodes++
		if current == nil || depth >= 8 || nodes > 32 || nilError(current) {
			return false
		}
		if multi, ok := current.(interface{ Unwrap() []error }); ok {
			children := multi.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		}
		if single, ok := current.(interface{ Unwrap() error }); ok {
			return visit(single.Unwrap(), depth+1)
		}
		leaves++
		for _, candidate := range allowed {
			if sameComparableError(current, candidate) {
				return true
			}
		}
		return false
	}
	return visit(err, 0) && leaves > 0
}

func nilError(err error) bool {
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func sameComparableError(left, right error) bool {
	leftType, rightType := reflect.TypeOf(left), reflect.TypeOf(right)
	return leftType != nil && leftType == rightType && leftType.Comparable() && left == right
}
