//go:build windows

package localdaemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

type diagnosticService struct {
	recorder *diagnostics.Recorder
	// snapshots is the daemon's snapshot source, so the bug report shows what pb status shows (F21-06).
	snapshots interface {
		Snapshot(context.Context) (localapi.Snapshot, error)
	}
	stateRoot string
	ownerSID  string
	clock     func() time.Time
}

func (s *diagnosticService) Diagnostics(context.Context) (localapi.DiagnosticSnapshot, error) {
	stats := s.recorder.Stats()
	return localapi.DiagnosticSnapshot{
		Schema:               localapi.DiagnosticSnapshotSchemaV1,
		ObservedAt:           s.clock().UTC(),
		Recent:               s.recorder.Recent(),
		DroppedRecords:       stats.DroppedRecords,
		DroppedBytes:         stats.DroppedBytes,
		FailedRecords:        stats.FailedRecords,
		PersistenceAvailable: stats.PersistenceAvailable,
	}, nil
}

func (s *diagnosticService) RecordBugreportMarker(ctx context.Context, phase string) error {
	if phase == "unexpected_cli_failure" || phase == "process_panic" {
		return s.recorder.RecordWithSupportReference("cli", phase, "error", supportref.FromContext(ctx), nil)
	}
	return s.recorder.RecordWithSupportReference("bugreport", "reproduction_marker", "info", supportref.FromContext(ctx), map[string]string{"phase": phase})
}

func (s *diagnosticService) CreateBugreport(ctx context.Context) (diagnostics.Bundle, error) {
	snapshot, err := s.snapshots.Snapshot(ctx)
	if err != nil {
		return diagnostics.Bundle{}, err
	}
	status, err := json.Marshal(snapshot)
	if err != nil {
		return diagnostics.Bundle{}, err
	}
	return diagnostics.CreateBundle(ctx, diagnostics.BundleConfig{
		Directory: filepath.Join(s.stateRoot, "bugreports"),
		OwnerSID:  s.ownerSID,
		OwnerUID:  -1,
		Recorder:  s.recorder,
		Status:    status,
		Clock:     s.clock,
	})
}
