package supportbundle

import (
	"context"
	"errors"
	"syscall"
	"testing"
)

type privateCollectorError struct{}

func (*privateCollectorError) Error() string { panic("private error must not be formatted") }

type collectorCycle struct{}

func (e *collectorCycle) Error() string { return "cycle" }
func (e *collectorCycle) Unwrap() error { return e }
func TestPartialCollectionRetainsPrivateCausesAndFreshRecovery(t *testing.T) {
	spy := &privateCollectorError{}
	failed := true
	b := mustBuilder(t, CollectorFunc{CollectorName: "evidence", CollectFunc: func(context.Context) ([]CollectedItem, error) {
		if failed {
			return nil, errors.Join(context.Canceled, syscall.EIO, spy)
		}
		return []CollectedItem{{Path: "health/ready", Kind: ItemKindText, Data: []byte("ready")}}, nil
	}}, CollectorFunc{CollectorName: "panic", CollectFunc: func(context.Context) ([]CollectedItem, error) {
		if failed {
			panic(spy)
		}
		return nil, nil
	}})
	p, err := b.Preview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cause := p.CollectionError()
	if !errors.Is(cause, syscall.EIO) || !errors.Is(cause, spy) {
		t.Fatal("lost causes")
	}
	if p.Manifest.Collectors[0].ErrorCode != ErrorCollectorFailed {
		t.Fatal("mixed cancellation quieted")
	}
	_ = cause.Error()
	failed = false
	p, err = b.Preview(t.Context())
	if err != nil || p.CollectionError() != nil || len(p.Manifest.Items) != 1 {
		t.Fatal("recovery failed")
	}
}
func TestCollectionAbortRetainsOperationalAndContextCauses(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	spy := &privateCollectorError{}
	b := mustBuilder(t, CollectorFunc{CollectorName: "blocked", CollectFunc: func(context.Context) ([]CollectedItem, error) { cancel(spy); return nil, syscall.EIO }})
	_, err := b.Preview(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) || !errors.Is(err, spy) {
		t.Fatal("lost abort cause")
	}
	_ = err.Error()
}
func TestCollectorCancellationRequiresCompleteContextLeaves(t *testing.T) {
	var nilCycle *collectorCycle
	for _, err := range []error{errors.Join(context.Canceled, syscall.EIO), &collectorCycle{}, nilCycle, errors.Join(context.Canceled, &collectorCycle{})} {
		if collectorContextCode(err) != ErrorCollectorFailed {
			t.Fatal("unresolved cause quieted")
		}
	}
	if collectorContextCode(errors.Join(context.Canceled, context.DeadlineExceeded)) != ErrorDeadlineExceeded {
		t.Fatal("deadline lost")
	}
}
func TestBundleCleanupRetainsPrimaryAndIndependentCause(t *testing.T) {
	original := &Error{Code: ErrorCanceled, Operation: "write support bundle", Cause: context.Canceled}
	err := retainCleanup(original, syscall.EIO)
	if errorCode(err) != ErrorCanceled || !errors.Is(err, original) || !errors.Is(err, syscall.EIO) {
		t.Fatal("lost cleanup cause")
	}
	_ = err.Error()
}
