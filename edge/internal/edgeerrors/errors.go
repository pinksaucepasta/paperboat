package edgeerrors

import (
	"fmt"
	"reflect"
)

type Code string

const (
	CodeConfigInvalid                   Code = "config_invalid"
	CodeCredentialReplayed              Code = "credential_replayed"
	CodeCredentialInvalid               Code = "credential_invalid"
	CodeCredentialMalformed             Code = "credential_malformed"
	CodeCredentialKeyUnavailable        Code = "credential_key_unavailable"
	CodeCredentialSignatureInvalid      Code = "credential_signature_invalid"
	CodeCredentialRevocationUnavailable Code = "credential_revocation_unavailable"
	CodeCredentialExpired               Code = "credential_expired"
	CodeCredentialNotYetValid           Code = "credential_not_yet_valid"
	CodeBindingInvalid                  Code = "credential_binding_invalid"
	CodeGenerationStale                 Code = "connector_generation_stale"
	CodeRevoked                         Code = "credential_revoked"
	CodeRunIDInvalid                    Code = "run_id_invalid"
	CodeRunIDMismatch                   Code = "run_id_mismatch"
	CodeRunIDExpired                    Code = "run_id_expired"
	CodeRunIDRevoked                    Code = "run_id_revoked"
	CodeOperationConflict               Code = "operation_conflict"
	CodeRouteConflict                   Code = "route_conflict"
	CodeRouteInvalid                    Code = "route_invalid"
	CodeRouteRevisionStale              Code = "route_revision_stale"
	CodeServiceUnavailable              Code = "service_unavailable"
	CodeStoreCapacity                   Code = "store_capacity_exceeded"
)

type Error struct {
	Code     Code
	Message  string
	Recovery string
	Cause    error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code Code, message, recovery string) *Error {
	return &Error{Code: code, Message: message, Recovery: recovery}
}

func Wrap(code Code, message, recovery string, cause error) *Error {
	return &Error{Code: code, Message: message, Recovery: recovery, Cause: cause}
}

func (c Code) Valid() bool {
	switch c {
	case CodeConfigInvalid, CodeCredentialReplayed, CodeCredentialInvalid, CodeCredentialMalformed, CodeCredentialKeyUnavailable, CodeCredentialSignatureInvalid, CodeCredentialRevocationUnavailable, CodeCredentialExpired, CodeCredentialNotYetValid, CodeBindingInvalid, CodeGenerationStale, CodeRevoked, CodeRunIDInvalid, CodeRunIDMismatch, CodeRunIDExpired, CodeRunIDRevoked, CodeOperationConflict, CodeRouteConflict, CodeRouteInvalid, CodeRouteRevisionStale, CodeServiceUnavailable, CodeStoreCapacity:
		return true
	default:
		return false
	}
}

func CodeOf(err error) (Code, bool) {
	pending := []error{err}
	seen := make(map[error]bool)
	for count := 0; len(pending) > 0 && count < 16; count++ {
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			continue
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			continue
		}
		if value.Type().Comparable() {
			if seen[current] {
				continue
			}
			seen[current] = true
		}
		if typed, ok := current.(*Error); ok && typed.Code.Valid() {
			return typed.Code, true
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) > 16-len(pending) {
				children = children[:16-len(pending)]
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		}
	}
	return "", false
}

func (e *Error) Format(s fmt.State, verb rune) { fmt.Fprint(s, e.Error()) }
