package diagnostics

import (
	"context"
	"errors"
	"time"
)

type Recorder struct {
	memory *MemoryRing
	disk   *DiskRing
	clock  func() time.Time
}

type recorderContextKey struct{}

// WithRecorder lends the process-owned recorder to components. The process
// owner closes it after all components and diagnostic producers have stopped.
func WithRecorder(ctx context.Context, recorder *Recorder) context.Context {
	return context.WithValue(ctx, recorderContextKey{}, recorder)
}

func FromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	recorder, _ := ctx.Value(recorderContextKey{}).(*Recorder)
	return recorder
}

// NewMemoryRecorder keeps bounded local evidence when persistent storage cannot
// be opened. The caller must expose the unavailable persistence state.
func NewMemoryRecorder() *Recorder {
	return &Recorder{memory: &MemoryRing{}, clock: time.Now}
}

func NewRecorder(config DiskConfig) (*Recorder, error) {
	disk, err := NewDiskRing(config)
	if err != nil {
		return nil, err
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Recorder{memory: &MemoryRing{}, disk: disk, clock: clock}, nil
}

func (r *Recorder) Record(category, code, severity string, fields map[string]string) error {
	return r.RecordWithSupportReference(category, code, severity, "", fields)
}

func (r *Recorder) RecordWithSupportReference(category, code, severity, reference string, fields map[string]string) error {
	if r == nil || r.memory == nil || r.clock == nil {
		return ErrInvalid
	}
	event, err := NewEventWithSupportReference(r.clock().UTC(), category, code, severity, reference, fields)
	if err != nil {
		return err
	}
	if r.disk == nil {
		return r.memory.Record(event)
	}
	return errors.Join(r.memory.Record(event), r.disk.Record(event))
}

func (r *Recorder) Recent() []Event {
	if r == nil || r.memory == nil {
		return nil
	}
	return r.memory.Snapshot()
}

func (r *Recorder) ReadDisk(ctx context.Context, maximum int64) ([]byte, error) {
	if r == nil || r.disk == nil {
		return nil, ErrInvalid
	}
	return r.disk.ReadAll(ctx, maximum)
}

func (r *Recorder) ReadDiskTail(ctx context.Context, maximum int64) ([]byte, error) {
	if r == nil || r.disk == nil {
		return nil, ErrInvalid
	}
	return r.disk.ReadTail(ctx, maximum)
}

func (r *Recorder) Flush(ctx context.Context) error {
	if r == nil {
		return ErrInvalid
	}
	if r.disk == nil {
		return nil
	}
	return r.disk.Flush(ctx)
}

func (r *Recorder) Stats() DiskStats {
	if r == nil || r.disk == nil {
		return DiskStats{}
	}
	return r.disk.Stats()
}

func (r *Recorder) Close() error {
	if r == nil || r.disk == nil {
		return nil
	}
	return r.disk.Close()
}

func (r *Recorder) CloseContext(ctx context.Context) error {
	if r == nil || r.disk == nil {
		return nil
	}
	return r.disk.CloseContext(ctx)
}
