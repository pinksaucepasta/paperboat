package workers

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

var progressNames = strings.Fields(`worker_machines worker_config_warnings worker_config_revocation worker_preview_leases worker_tunnel_expiry worker_connector_rotation worker_connector_drain worker_domain_dns worker_preview_dns worker_preview_outbox worker_team_retention worker_team_entitlements worker_team_billing worker_team_inbox worker_edge_assignments worker_stale_nodes worker_edge_dns worker_diagnostic_uploads worker_certificates worker_platform_certificates`)

const (
	ProgressUnknown = iota
	ProgressCurrent
	ProgressRunning
	ProgressFailed
	ProgressStale
	ProgressCanceled
)

type ProgressSample struct {
	Name         string
	State        int
	LastFinished time.Time
	AgeSeconds   *float64
	Warming      bool
	Healthy      bool
}
type progressEntry struct {
	registered, started, finished time.Time
	interval                      time.Duration
	state                         int
	iterationFailed               bool
	lastSucceeded                 bool
	iterationFailure              *IterationFault
}
type Progress struct {
	mu      sync.Mutex
	entries map[string]*progressEntry
	stall   time.Duration
	now     func() time.Time
}
type progressContextKey struct{}
type progressContext struct {
	owner    *Progress
	name     string
	observer IterationObserver
}

// IterationFault is the first retryable failure observed during one real
// worker pass. Err is held only until that pass finishes.
type IterationFault struct {
	Stage string
	Code  string
	Err   error
}

// IterationObserver runs after the progress lock is released. Observers own
// deduplication and project Err before retaining or emitting metadata.
type IterationObserver func(context.Context, IterationFault, bool)

func NewProgress(stall time.Duration) *Progress {
	if stall < time.Minute || stall > 24*time.Hour {
		stall = 30 * time.Minute
	}
	return &Progress{entries: make(map[string]*progressEntry), stall: stall, now: time.Now}
}
func (p *Progress) Register(name string) {
	if p == nil || !slices.Contains(progressNames, name) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.entries[name]; !exists && len(p.entries) < 32 {
		p.entries[name] = &progressEntry{registered: p.now()}
	}
}
func (p *Progress) Context(ctx context.Context, name string, observer ...IterationObserver) context.Context {
	if p == nil {
		return ctx
	}
	var callback IterationObserver
	if len(observer) > 0 {
		callback = observer[0]
	}
	return context.WithValue(ctx, progressContextKey{}, progressContext{owner: p, name: name, observer: callback})
}

// BeginIteration is called around real reconciliation work, never from a timer
// heartbeat. Empty, successfully reconciled batches are genuine progress.
func BeginIteration(ctx context.Context, interval time.Duration) func(error) {
	v, ok := ctx.Value(progressContextKey{}).(progressContext)
	if !ok {
		return func(error) {}
	}
	p := v.owner
	p.mu.Lock()
	e := p.entries[v.name]
	if e == nil {
		p.mu.Unlock()
		return func(error) {}
	}
	e.started = p.now()
	e.interval = interval
	e.state = ProgressRunning
	e.iterationFailed = false
	e.iterationFailure = nil
	p.mu.Unlock()
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			var notify *IterationFault
			recovered := false
			p.mu.Lock()
			wasFailed := !e.finished.IsZero() && !e.lastSucceeded
			e.finished = p.now()
			e.state = ProgressCurrent
			if err != nil || e.iterationFailed {
				e.state = ProgressFailed
			}
			ctxErr := ctx.Err()
			canceled := errors.Is(ctxErr, context.Canceled)
			if canceled {
				e.state = ProgressCanceled
			} else if errors.Is(ctxErr, context.DeadlineExceeded) {
				e.state = ProgressFailed
			}
			if e.iterationFailure != nil {
				failure := *e.iterationFailure
				if canceled {
					if contextCancellationOnly(failure.Err) {
						failure = IterationFault{Stage: failure.Stage, Code: "worker_canceled", Err: context.Canceled}
					} else {
						failure.Err = errors.Join(failure.Err, context.Canceled)
						e.state = ProgressFailed
					}
				}
				notify = &failure
			} else if wasFailed && e.state == ProgressCurrent {
				notify = &IterationFault{Stage: "worker_recovery", Code: "worker_recovered"}
				recovered = true
			}
			e.lastSucceeded = e.state == ProgressCurrent
			callback := v.observer
			e.iterationFailure = nil
			p.mu.Unlock()
			if notify != nil && callback != nil {
				callback(ctx, *notify, recovered)
			}
		})
	}
}

// contextCancellationOnly accepts a bounded complete error tree containing
// only context.Canceled leaves. Joined operational failures must remain visible
// when worker shutdown races an in-flight iteration.
func contextCancellationOnly(err error) (only bool) {
	defer func() {
		if recover() != nil {
			only = false
		}
	}()
	if err == nil {
		return false
	}
	nodes := 0
	var visit func(error, int) bool
	visit = func(current error, depth int) bool {
		if current == nil || depth >= 8 || nodes >= 16 {
			return false
		}
		typ := reflect.TypeOf(current)
		value := reflect.ValueOf(current)
		if typ == nil || (value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface || value.Kind() == reflect.Map || value.Kind() == reflect.Slice || value.Kind() == reflect.Func) && value.IsNil() {
			return false
		}
		nodes++
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			child := wrapped.Unwrap()
			return child != nil && visit(child, depth+1)
		}
		if !typ.Comparable() || typ != reflect.TypeOf(context.Canceled) {
			return false
		}
		return current == context.Canceled
	}
	return visit(err, 0)
}

// IterationFailure records the first retryable error in the current pass. The
// raw error is released as soon as the pass finishes and never enters a
// progress snapshot.
func IterationFailure(ctx context.Context, stage, code string, err error) {
	if err == nil {
		return
	}
	v, ok := ctx.Value(progressContextKey{}).(progressContext)
	if !ok {
		return
	}
	v.owner.mu.Lock()
	if e := v.owner.entries[v.name]; e != nil {
		e.iterationFailed = true
		if e.iterationFailure == nil {
			if !safeFailureDimension(stage) {
				stage = "worker_iteration"
			}
			if !safeFailureDimension(code) {
				code = "worker_iteration_failed"
			}
			e.iterationFailure = &IterationFault{Stage: stage, Code: code, Err: err}
		}
	}
	v.owner.mu.Unlock()
}

func safeFailureDimension(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return true
}
func (p *Progress) Exit(name string, canceled bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[name]; e != nil {
		e.state = ProgressFailed
		if canceled {
			e.state = ProgressCanceled
		}
	}
}
func (p *Progress) Snapshot() []ProgressSample {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]ProgressSample, 0, len(p.entries))
	for name, e := range p.entries {
		s := ProgressSample{Name: name, State: e.state, LastFinished: e.finished}
		if !e.finished.IsZero() {
			age := now.Sub(e.finished).Seconds()
			if age < 0 {
				age = 0
			}
			s.AgeSeconds = &age
		}
		switch e.state {
		case ProgressUnknown:
			if now.Sub(e.registered) > p.stall {
				s.State = ProgressStale
			} else {
				s.Warming = true
			}
		case ProgressRunning:
			if now.Sub(e.started) > p.stall {
				s.State = ProgressStale
			} else if e.finished.IsZero() {
				s.Warming = true
			} else if !e.lastSucceeded {
				s.State = ProgressFailed
			}
		case ProgressCurrent:
			if now.Sub(e.finished) > 5*e.interval+time.Minute {
				s.State = ProgressStale
			}
		}
		s.Healthy = s.State == ProgressCurrent || s.State == ProgressRunning && !s.Warming && e.lastSucceeded
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func ProgressNames() []string { return slices.Clone(progressNames) }
