package bandwidth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

const maxCounters = 8192
const maxStateBytes = 8 << 20
const retention = 7 * 24 * time.Hour

type Sink interface {
	ReportNativeUsage(context.Context, []api.NativeUsageReport, int64, int64, bool) error
}
type counter struct {
	Report api.NativeUsageReport `json:"report"`
	Dirty  bool                  `json:"dirty"`
}
type State struct {
	Counters        map[string]counter `json:"counters"`
	UnrecordedBytes int64              `json:"unrecorded_bytes"`
	ExpiredBytes    int64              `json:"expired_bytes"`
	LastDelivered   time.Time          `json:"last_delivered"`
	Incomplete      bool               `json:"incomplete"`
	CleanShutdown   bool               `json:"clean_shutdown"`
}
type Recorder struct {
	mu           sync.Mutex
	flushPermit  chan struct{}
	state        State
	path         string
	sink         Sink
	observeError func(error)
	cancel       context.CancelFunc
	done         chan struct{}
}

// Open recovers pending absolute counters. Only one recorder owns a host's
// state file; listeners finish before their owning host drains this recorder.
func Open(path string, sink Sink, observeError func(error)) (*Recorder, error) {
	if sink == nil || path == "" {
		return nil, errors.New("invalid bandwidth recorder")
	}
	r := &Recorder{path: path, sink: sink, observeError: observeError, state: State{Counters: map[string]counter{}}, done: make(chan struct{}), flushPermit: make(chan struct{}, 1)}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() > maxStateBytes {
			return nil, errors.New("invalid bandwidth state file")
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil, recorderFailure{"diagnostic_storage", openErr}
		}
		body, readErr := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
		decodeErr := json.Unmarshal(body, &r.state)
		closeErr := f.Close()
		if readErr != nil || closeErr != nil {
			return nil, recorderFailure{"diagnostic_storage", errors.Join(readErr, closeErr)}
		}
		if len(body) > maxStateBytes {
			return nil, recorderFailure{"diagnostic_storage", errors.New("bandwidth state exceeds storage limit")}
		}
		if decodeErr != nil {
			if err := atomicfile.Write(path+".corrupt", body, atomicfile.CurrentOwnerOptions(0o600)); err != nil {
				return nil, recorderFailure{"diagnostic_storage", err}
			}
			r.state = State{Counters: map[string]counter{}, Incomplete: true}
		}
		if !r.state.CleanShutdown {
			r.state.Incomplete = true
		}
		if len(r.state.Counters) > maxCounters {
			return nil, errors.New("bandwidth state exceeds counter limit")
		}
		if r.state.Counters == nil {
			r.state.Counters = map[string]counter{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, recorderFailure{"diagnostic_storage", err}
	}
	r.state.CleanShutdown = false
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				request, stop := context.WithTimeout(ctx, 5*time.Second)
				err := r.Flush(request)
				stop()
				if ctx.Err() == nil && r.observeError != nil {
					r.observeError(err)
				}
			}
		}
	}()
	return r, nil
}

func key(report api.NativeUsageReport) string {
	value, _ := json.Marshal([]string{report.AccessSessionID, report.StreamID, report.Consumer, report.Mode, report.NodeID, report.IntervalStart.Format(time.RFC3339)})
	return fmt.Sprintf("%x", sha256.Sum256(value))
}

func (r *Recorder) add(binding Binding, path Path, n int64, upload bool, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := now.Truncate(time.Minute)
	report := api.NativeUsageReport{AccessSessionID: binding.AccessSessionID, StreamID: binding.StreamID, Consumer: binding.Consumer, Mode: path.Mode, NodeID: path.NodeID, IntervalStart: start, IntervalEnd: now}
	id := key(report)
	value, exists := r.state.Counters[id]
	if !exists && len(r.state.Counters) >= maxCounters {
		// Evict only acknowledged, completed buckets. Pending bytes must not be
		// silently replaced when the control plane is unavailable.
		for k, c := range r.state.Counters {
			if !c.Dirty && c.Report.IntervalStart.Before(start) {
				delete(r.state.Counters, k)
			}
		}
		if len(r.state.Counters) >= maxCounters {
			r.state.UnrecordedBytes += n
			return
		}
	}
	if !exists {
		value.Report = report
	}
	if now.After(value.Report.IntervalEnd) {
		value.Report.IntervalEnd = now
	}
	if upload {
		value.Report.UploadBytes += n
	} else {
		value.Report.DownloadBytes += n
	}
	value.Dirty = true
	r.state.Counters[id] = value
}

func (r *Recorder) persistLocked() error {
	value, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	if len(value) > maxStateBytes {
		return errors.New("bandwidth state exceeds storage limit")
	}
	return atomicfile.Write(r.path, value, atomicfile.CurrentOwnerOptions(0o600))
}

func (r *Recorder) Flush(ctx context.Context) error {
	return r.flush(ctx, false)
}

func (r *Recorder) flush(ctx context.Context, cleanShutdown bool) error {
	select {
	case r.flushPermit <- struct{}{}:
		defer func() { <-r.flushPermit }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if cleanShutdown {
		r.state.CleanShutdown = true
	}
	now := time.Now().UTC()
	for id, c := range r.state.Counters {
		if now.Sub(c.Report.IntervalStart) > retention {
			if c.Dirty {
				r.state.ExpiredBytes += c.Report.UploadBytes + c.Report.DownloadBytes
			}
			delete(r.state.Counters, id)
		}
	}
	if err := r.persistLocked(); err != nil {
		r.mu.Unlock()
		return recorderFailure{"diagnostic_storage", err}
	}
	keys := make([]string, 0, len(r.state.Counters))
	for id, c := range r.state.Counters {
		if c.Dirty {
			keys = append(keys, id)
		}
	}
	sort.Strings(keys)
	r.mu.Unlock()
	if len(keys) == 0 {
		status := r.Snapshot()
		if err := r.sink.ReportNativeUsage(ctx, []api.NativeUsageReport{}, status.UnrecordedBytes, status.ExpiredBytes, status.Incomplete); err != nil {
			return recorderFailure{"control_request", err}
		}
		return nil
	}
	for len(keys) > 0 {
		count := min(len(keys), 64)
		batch := make([]api.NativeUsageReport, 0, count)
		r.mu.Lock()
		for _, id := range keys[:count] {
			batch = append(batch, r.state.Counters[id].Report)
		}
		r.mu.Unlock()
		status := r.Snapshot()
		if err := r.sink.ReportNativeUsage(ctx, batch, status.UnrecordedBytes, status.ExpiredBytes, status.Incomplete); err != nil {
			return recorderFailure{"control_request", err}
		}
		r.mu.Lock()
		for i, id := range keys[:count] {
			value := r.state.Counters[id]
			if value.Report == batch[i] {
				value.Dirty = false
				r.state.Counters[id] = value
			}
		}
		r.state.LastDelivered = time.Now().UTC()
		err := r.persistLocked()
		r.mu.Unlock()
		if err != nil {
			return recorderFailure{"diagnostic_storage", err}
		}
		keys = keys[count:]
	}
	return nil
}

func (r *Recorder) Close(ctx context.Context) error {
	r.cancel()
	select {
	case <-r.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.flush(ctx, true)
}

func (r *Recorder) Snapshot() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return State{UnrecordedBytes: r.state.UnrecordedBytes, ExpiredBytes: r.state.ExpiredBytes, LastDelivered: r.state.LastDelivered, Incomplete: r.state.Incomplete}
}

// recorderFailure retains a retry's original cause without exposing state paths
// or server response contents to callers or diagnostic presentation.
type recorderFailure struct {
	stage string
	cause error
}

func (recorderFailure) Error() string             { return "bandwidth reporting failed" }
func (f recorderFailure) Unwrap() error           { return f.cause }
func (f recorderFailure) DiagnosticStage() string { return f.stage }
func (f recorderFailure) DiagnosticCode() string {
	if f.stage == "diagnostic_storage" {
		return "diagnostic_storage_unavailable"
	}
	return "control_request_failed"
}
