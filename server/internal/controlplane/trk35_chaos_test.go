package controlplane

import (
	"errors"
	"testing"
)

func TestTRK35StaleControlWorkerCompletionReportsLeaseLoss(t *testing.T) {
	storageErr := errors.New("storage unavailable")
	for _, test := range []struct {
		name    string
		updated int64
		err     error
		want    error
	}{
		{name: "newer worker won", updated: 0, want: ErrOperationLeaseLost},
		{name: "owner completed", updated: 1},
		{name: "duplicate affected rows", updated: 2},
		{name: "storage error", updated: 0, err: storageErr, want: storageErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := operationUpdateResult(test.updated, test.err); !errors.Is(got, test.want) {
				t.Fatalf("operation result = %v, want %v", got, test.want)
			}
		})
	}
}
