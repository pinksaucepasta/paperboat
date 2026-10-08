package main

import "reflect"

type commandRejectionReason uint8

const (
	commandRejectDefaultSessionRename commandRejectionReason = iota + 1
	commandRejectDefaultSessionDelete
	commandRejectOpenSessionDelete
	commandRejectMissingServer
	commandRejectNoninteractiveHome
)

// commandRejection describes a command-owned refusal before mutation. Its
// finite reason controls presentation; an optional original cause still
// participates in command classification and can outrank the refusal.
type commandRejection struct {
	reason commandRejectionReason
	cause  error
}

func (e commandRejection) valid() bool {
	return e.reason >= commandRejectDefaultSessionRename && e.reason <= commandRejectNoninteractiveHome
}
func (e commandRejection) Error() string {
	switch e.reason {
	case commandRejectDefaultSessionRename:
		return "The default session cannot be renamed. Select another session."
	case commandRejectDefaultSessionDelete:
		return "The default session cannot be deleted. Select another closed session."
	case commandRejectOpenSessionDelete:
		return "close the terminal session before deleting its record."
	case commandRejectMissingServer:
		return "Paperboat server is not configured; set server_url or use --server."
	case commandRejectNoninteractiveHome:
		return "pb requires a command or environment when used without an interactive terminal"
	}
	return "Paperboat could not finish the command. Check the current state before retrying."
}
func (e commandRejection) Unwrap() error { return e.cause }

// API response status is supporting metadata, not permission to ignore an
// independently preserved decoder/read failure. This proof is limited to the
// private body-free status cause emitted by the API transport boundary.
func commandAPIStatusMetadataOnly(err error, status int) (pure bool) {
	defer func() {
		if recover() != nil {
			pure = false
		}
	}()
	remaining := 32
	var visit func(error) bool
	visit = func(current error) bool {
		if current == nil || remaining == 0 {
			return false
		}
		remaining--
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
			if value.IsNil() {
				return false
			}
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > remaining {
				return false
			}
			for _, child := range children {
				if !visit(child) {
					return false
				}
			}
			return true
		}
		if unary, ok := current.(interface{ Unwrap() error }); ok {
			return visit(unary.Unwrap())
		}
		metadata, ok := current.(interface{ DiagnosticStatus() int })
		return ok && status >= 400 && status <= 599 && metadata.DiagnosticStatus() == status
	}
	return visit(err)
}
