package localdaemon

import (
	"context"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

func peerProbeFailure(ctx context.Context, err error) error {
	if nativeProbeAuthorityDenied(err) {
		errorreport.Current().Observe(ctx, "paperboatd", "peer_probe", "rejected", -1)
		return localapi.PermissionFailure(err)
	}
	if !errorreport.HTTPAttemptObserved(err) {
		errorreport.Current().ObserveFailure(ctx, "paperboatd", "peer_probe", "peer_connect", "transport_failed", err)
	}
	return err
}

func nativeProbeAuthorityDenied(err error) bool {
	pending := []error{err}
	denied := false
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if value.Type().Comparable() {
			if current == tailnet.ErrAdmission || current == tailnet.ErrAuthority {
				denied = true
				continue
			}
			if current == context.Canceled {
				continue
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 15-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		default:
			return false
		}
	}
	return denied
}
