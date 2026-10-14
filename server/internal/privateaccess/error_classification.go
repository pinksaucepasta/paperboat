package privateaccess

import "reflect"

const (
	maxPrivateAccessErrorDepth = 8
	maxPrivateAccessErrorNodes = 16
)

// pureExpectedPrivateAccessError accepts only a bounded error tree whose
// leaves are exact expected sentinels. A sentinel joined with an operational
// or unknown error remains a failure.
func pureExpectedPrivateAccessError(err error, expected ...error) (accepted bool) {
	defer func() {
		if recover() != nil {
			accepted = false
		}
	}()
	if err == nil || len(expected) == 0 {
		return false
	}

	nodes := 0
	matched := false
	var visit func(error, int) bool
	visit = func(current error, depth int) bool {
		if current == nil || depth >= maxPrivateAccessErrorDepth || nodes >= maxPrivateAccessErrorNodes {
			return false
		}
		typ := reflect.TypeOf(current)
		value := reflect.ValueOf(current)
		if typ == nil || (value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface || value.Kind() == reflect.Map || value.Kind() == reflect.Slice || value.Kind() == reflect.Func) && value.IsNil() {
			return false
		}
		nodes++
		if typ.Comparable() {
			for _, sentinel := range expected {
				sentinelType := reflect.TypeOf(sentinel)
				if sentinel != nil && sentinelType != nil && sentinelType.Comparable() && typ == sentinelType && current == sentinel {
					matched = true
					return true
				}
			}
		}

		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
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
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			child := wrapped.Unwrap()
			return child != nil && visit(child, depth+1)
		}
		return false
	}
	accepted = visit(err, 0) && matched
	return accepted
}

// privateAccessDenial follows only a single wrapper chain to its typed denial.
// Joined failures must stay with the operational owner instead of being
// projected as one of their expected denial branches.
func privateAccessDenial(err error) (denied *DeniedError, ok bool) {
	defer func() {
		if recover() != nil {
			denied, ok = nil, false
		}
	}()
	for depth := 0; err != nil && depth < maxPrivateAccessErrorDepth; depth++ {
		value := reflect.ValueOf(err)
		if value.Kind() == reflect.Ptr && value.IsNil() {
			return nil, false
		}
		if current, isDenied := err.(*DeniedError); isDenied {
			if current == nil || !current.Reason.valid() {
				return nil, false
			}
			return current, true
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return nil, false
		}
		wrapped, hasCause := err.(interface{ Unwrap() error })
		if !hasCause {
			return nil, false
		}
		err = wrapped.Unwrap()
	}
	return nil, false
}

func privateAccessExpectedDenial(err error) (*DeniedError, bool) {
	denied, ok := privateAccessDenial(err)
	if !ok || denied.DiagnosticCause() != nil || !pureExpectedPrivateAccessError(err, ErrDenied) {
		return nil, false
	}
	return denied, true
}
