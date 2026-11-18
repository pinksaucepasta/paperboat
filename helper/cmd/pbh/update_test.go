package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type updateRecord struct {
	states             []string
	detail, reference  string
	backedUp, restored bool
	installed          bool
}

func fakeUpdate(record *updateRecord, verifyErr, installErr error) updateSteps {
	return updateSteps{
		report: func(state, detail, reference string) {
			record.states = append(record.states, state)
			if detail != "" {
				record.detail, record.reference = detail, reference
			}
		},
		download: func(context.Context) (string, error) { return "/bundle", nil },
		verify:   func(context.Context, string) error { return verifyErr },
		extract:  func(string) (string, error) { return "/pbh", nil },
		backup: func() (func() error, error) {
			record.backedUp = true
			return func() error { record.restored = true; return nil }, nil
		},
		install: func(context.Context, string) error { record.installed = true; return installErr },
	}
}

// An update reports every step; a new version that fails its health check is rolled back with the exact reason and
// a support reference; a release whose signature does not verify is refused before anything on the host changes.
func TestRunUpdateSucceedsRollsBackAndRefusesUnsigned(t *testing.T) {
	var ok updateRecord
	if err := runUpdate(context.Background(), fakeUpdate(&ok, nil, nil)); err != nil || strings.Join(ok.states, ",") != "downloading,verifying,swapping,checking,succeeded" || ok.restored {
		t.Fatalf("success: %v %+v", err, ok)
	}

	var unhealthy updateRecord
	err := runUpdate(context.Background(), fakeUpdate(&unhealthy, nil, errors.New("edge not ready in 120s")))
	if err == nil || !unhealthy.restored || unhealthy.states[len(unhealthy.states)-1] != "rolled_back" ||
		!strings.Contains(unhealthy.detail, "edge not ready in 120s") || !strings.HasPrefix(unhealthy.reference, "support_") {
		t.Fatalf("failed health check: %v %+v", err, unhealthy)
	}

	var unsigned updateRecord
	err = runUpdate(context.Background(), fakeUpdate(&unsigned, errors.New("sha256 differs"), nil))
	if err == nil || unsigned.backedUp || unsigned.installed || unsigned.states[len(unsigned.states)-1] != "failed" || !strings.Contains(unsigned.detail, "signature") {
		t.Fatalf("unsigned release: %v %+v", err, unsigned)
	}
}
