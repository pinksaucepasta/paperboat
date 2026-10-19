package networkrecovery

import (
	"context"
	"reflect"
	"syscall"
)

// FailureClass is the stable, secret-free retry/health dimension.
type FailureClass string

const (
	FailureAuthentication FailureClass = "authentication"
	FailureConfiguration  FailureClass = "configuration"
	FailureDNS            FailureClass = "dns"
	FailureOrigin         FailureClass = "origin"
	FailureEdge           FailureClass = "edge"
	FailureLocalSystem    FailureClass = "local_system"
	FailureTimeout        FailureClass = "timeout"
	FailureCanceled       FailureClass = "canceled"
	FailureUnknown        FailureClass = "unknown"
)

// Failure is a typed error with a stable retry decision. Cause is retained for
// errors.Is/errors.As by the caller but is never copied into HealthSnapshot.
type Failure struct {
	Class     FailureClass
	Permanent bool
	Cause     error
}

func (f Failure) Error() string {
	switch f.Class {
	case FailureAuthentication, FailureConfiguration, FailureDNS, FailureOrigin,
		FailureEdge, FailureLocalSystem, FailureTimeout, FailureCanceled:
		return "network carrier " + string(f.Class) + " failure"
	default:
		return "network carrier recovery failed"
	}
}

func (f Failure) Unwrap() error { return f.Cause }

func (f Failure) FailureClass() FailureClass { return f.Class }

func (f Failure) PermanentFailure() bool { return f.Permanent }

type failureMarker interface {
	FailureClass() FailureClass
	PermanentFailure() bool
}

// Classify maps transport/platform errors into the bounded retry policy.
func Classify(err error) Failure {
	if err == nil {
		return Failure{}
	}
	budget := 16
	active := make(map[error]bool)
	class, permanent := classifyCause(err, &budget, active, Failure{})
	return Failure{Class: class, Permanent: permanent, Cause: err}
}

// Only complete bounded cause trees can authorize quiet cancellation or a
// permanent decision. Independent unknown causes cannot be hidden by a marker.
func classifyCause(err error, budget *int, active map[error]bool, inherited Failure) (FailureClass, bool) {
	if err == nil || *budget == 0 {
		return FailureUnknown, false
	}
	*budget--
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		if value.IsNil() {
			return FailureUnknown, false
		}
	}
	if value.Type().Comparable() {
		if active[err] {
			return FailureUnknown, false
		}
		active[err] = true
		defer delete(active, err)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 || len(children) > *budget {
			return FailureUnknown, false
		}
		class, permanent := FailureCanceled, true
		for _, child := range children {
			childClass, childPermanent := classifyCause(child, budget, active, Failure{})
			if childClass == FailureCanceled {
				continue
			}
			if class == FailureCanceled {
				class, permanent = childClass, childPermanent
				continue
			}
			if class != childClass {
				class, permanent = FailureUnknown, false
			} else {
				permanent = permanent && childPermanent
			}
		}
		return class, permanent
	}
	if wrapper, ok := err.(interface{ Unwrap() error }); ok {
		if child := wrapper.Unwrap(); child != nil {
			if marker, ok := err.(failureMarker); ok && (inherited.Class == "" || inherited.Class == FailureCanceled) {
				switch class := marker.FailureClass(); class {
				case FailureAuthentication, FailureConfiguration, FailureDNS, FailureOrigin, FailureEdge, FailureLocalSystem, FailureTimeout, FailureCanceled:
					inherited = Failure{Class: class, Permanent: marker.PermanentFailure()}
				default:
					inherited = Failure{}
				}
			}
			return classifyCause(child, budget, active, inherited)
		}
	}
	// A complete unary chain retains the first backend's explicit non-cancel
	// retry decision. Joins deliberately discard that authority above; invalid,
	// cyclic and truncated trees already returned unknown before reaching a leaf.
	if inherited.Class != "" && inherited.Class != FailureCanceled {
		return inherited.Class, inherited.Permanent
	}
	if marker, ok := err.(failureMarker); ok {
		switch class := marker.FailureClass(); class {
		case FailureAuthentication, FailureConfiguration, FailureDNS, FailureOrigin, FailureEdge, FailureLocalSystem, FailureTimeout:
			return class, marker.PermanentFailure()
		}
		return FailureUnknown, false
	}
	switch err {
	case context.Canceled:
		return FailureCanceled, true
	case context.DeadlineExceeded, ErrTimeout:
		return FailureTimeout, false
	case ErrAuthentication:
		return FailureAuthentication, true
	case ErrConfiguration, ErrIdentityChanged:
		return FailureConfiguration, true
	case ErrReplacementNotReady, ErrLocalSystem:
		return FailureLocalSystem, false
	case ErrDNS:
		return FailureDNS, false
	case ErrOrigin:
		return FailureOrigin, false
	case ErrEdge:
		return FailureEdge, false
	default:
		if _, native := err.(syscall.Errno); native {
			return FailureLocalSystem, false
		}
		return FailureUnknown, false
	}
}

func failureCode(class FailureClass) string {
	switch class {
	case FailureAuthentication:
		return "carrier_authentication_failed"
	case FailureConfiguration:
		return "carrier_configuration_failed"
	case FailureDNS:
		return "carrier_dns_failed"
	case FailureOrigin:
		return "carrier_origin_failed"
	case FailureEdge:
		return "carrier_edge_failed"
	case FailureTimeout:
		return "carrier_timeout"
	case FailureLocalSystem:
		return "carrier_local_system_failed"
	case FailureCanceled:
		return "carrier_canceled"
	default:
		return "carrier_failed"
	}
}
