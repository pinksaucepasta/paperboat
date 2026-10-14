package diagnostics

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"path/filepath"
	"time"
)

const (
	BundleSchemaV1     = "paperboat.diagnostic-bundle/v1"
	MaximumBundleBytes = 25 << 20
	maximumEventExport = 20 << 20
	maximumStatusBytes = 1 << 20
)

type BundleConfig struct {
	Directory string
	OwnerSID  string
	OwnerUID  int
	Recorder  *Recorder
	Status    json.RawMessage
	Clock     func() time.Time
}

type Bundle struct {
	Schema      string    `json:"schema"`
	Correlation string    `json:"correlation"`
	CreatedAt   time.Time `json:"created_at"`
	Path        string    `json:"path"`
	Bytes       int64     `json:"bytes"`
	Categories  []string  `json:"categories"`
}

func (b Bundle) Validate() error {
	if b.Schema != BundleSchemaV1 || !supportref.Valid(b.Correlation) || b.CreatedAt.IsZero() || b.CreatedAt.Location() != time.UTC || !filepath.IsAbs(b.Path) || filepath.Clean(b.Path) != b.Path || b.Bytes <= 0 || b.Bytes > MaximumBundleBytes || len(b.Categories) != 4 {
		return ErrInvalid
	}
	want := []string{"manifest", "recent_events", "redacted_events", "status"}
	for index := range want {
		if b.Categories[index] != want[index] {
			return ErrInvalid
		}
	}
	return nil
}

type bundleManifest struct {
	PersistentFlushSucceeded  bool      `json:"persistent_flush_succeeded"`
	PersistentEventsAvailable bool      `json:"persistent_events_available"`
	PersistenceError          string    `json:"persistence_error,omitempty"`
	PersistenceAvailable      bool      `json:"persistence_available"`
	FailedRecords             uint64    `json:"failed_records"`
	Schema                    string    `json:"schema"`
	Correlation               string    `json:"correlation"`
	CreatedAt                 time.Time `json:"created_at"`
	Categories                []string  `json:"categories"`
	DroppedRecords            uint64    `json:"dropped_records"`
	DroppedBytes              uint64    `json:"dropped_bytes"`
}

func CreateBundle(ctx context.Context, config BundleConfig) (Bundle, error) {
	if ctx == nil || config.Recorder == nil || !filepath.IsAbs(config.Directory) || filepath.Clean(config.Directory) != config.Directory || len(config.Status) == 0 || len(config.Status) > maximumStatusBytes || !json.Valid(config.Status) {
		return Bundle{}, ErrInvalid
	}
	owner, err := resolveDiagnosticOwner(DiskConfig{OwnerUID: config.OwnerUID, OwnerSID: config.OwnerSID})
	if err != nil {
		return Bundle{}, err
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if err := ensureDiagnosticDirectory(config.Directory, owner); err != nil {
		return Bundle{}, err
	}
	correlation, err := newCorrelation()
	if err != nil {
		return Bundle{}, err
	}
	createdAt := config.Clock().UTC()
	flushErr := config.Recorder.Flush(ctx)
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	var events []byte
	var readErr error
	if config.Recorder.disk != nil {
		events, readErr = config.Recorder.ReadDiskTail(ctx, maximumEventExport)
	}
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	// Storage failure must not prevent exporting the bounded memory evidence.
	// The manifest exposes missing persistence rather than implying completeness.
	persistenceError := ""
	storageErr := flushErr
	if readErr != nil {
		storageErr = readErr
	}
	if storageErr != nil {
		persistenceError = errorreport.ProjectFault(ctx, "pb", "bugreport", "diagnostic_storage", "diagnostic_storage_unavailable", storageErr).Cause
	}
	recent, err := marshalEvents(config.Recorder.Recent())
	if err != nil {
		return Bundle{}, err
	}
	categories := []string{"manifest", "recent_events", "redacted_events", "status"}
	stats := config.Recorder.Stats()
	manifest, err := json.Marshal(bundleManifest{PersistentFlushSucceeded: config.Recorder.disk != nil && flushErr == nil, PersistentEventsAvailable: config.Recorder.disk != nil && readErr == nil, PersistenceError: persistenceError, Schema: BundleSchemaV1, Correlation: correlation, CreatedAt: createdAt, Categories: categories, PersistenceAvailable: stats.PersistenceAvailable, FailedRecords: stats.FailedRecords, DroppedRecords: stats.DroppedRecords, DroppedBytes: stats.DroppedBytes})
	if err != nil {
		return Bundle{}, err
	}
	var output boundedBundleBuffer
	archive := zip.NewWriter(&output)
	for _, entry := range []struct {
		name string
		data []byte
	}{{"manifest.json", append(manifest, '\n')}, {"recent-events.ndjson", recent}, {"events.ndjson", events}, {"status.json", append(bytes.TrimSpace(config.Status), '\n')}} {
		writer, createErr := archive.CreateHeader(&zip.FileHeader{Name: entry.name, Method: zip.Deflate})
		if createErr != nil {
			return Bundle{}, createErr
		}
		if _, writeErr := writer.Write(entry.data); writeErr != nil {
			return Bundle{}, writeErr
		}
	}
	if err := archive.Close(); err != nil {
		return Bundle{}, err
	}
	if output.err != nil || output.Len() == 0 || output.Len() > MaximumBundleBytes {
		return Bundle{}, ErrInvalid
	}
	finalPath := filepath.Join(config.Directory, "bugreport-"+correlation+".zip")
	if err := writeDiagnosticAtomic(finalPath, output.Bytes(), owner); err != nil {
		return Bundle{}, err
	}
	info, err := verifiedDiagnosticFile(finalPath, owner)
	if err != nil || info.Size() != int64(output.Len()) || info.Size() > MaximumBundleBytes {
		_ = removeDiagnosticFile(finalPath, owner)
		return Bundle{}, ErrInvalid
	}
	return Bundle{Schema: BundleSchemaV1, Correlation: correlation, CreatedAt: createdAt, Path: finalPath, Bytes: info.Size(), Categories: categories}, nil
}

type boundedBundleBuffer struct {
	bytes.Buffer
	err error
}

func (b *boundedBundleBuffer) Write(value []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if len(value) > MaximumBundleBytes-b.Len() {
		b.err = ErrInvalid
		return 0, b.err
	}
	return b.Buffer.Write(value)
}

func safeHex(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func marshalEvents(events []Event) ([]byte, error) {
	var result bytes.Buffer
	for _, event := range events {
		if event.Validate() != nil {
			return nil, ErrInvalid
		}
		encoded, err := json.Marshal(event)
		if err != nil || result.Len()+len(encoded)+1 > MaximumRecordBytes*MemoryCapacity {
			return nil, ErrInvalid
		}
		result.Write(encoded)
		result.WriteByte('\n')
	}
	return result.Bytes(), nil
}

func newCorrelation() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return "support_" + id.String(), nil
}
