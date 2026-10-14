package autoupdate

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

var ErrInvalidConfig = errors.New("automatic update scheduler configuration is invalid")

const (
	DefaultInterval   = 6 * time.Hour
	DefaultRetryFloor = 5 * time.Minute
	DefaultRetryLimit = 6 * time.Hour
)

type Result struct {
	Version string
	Updated bool
}

type Check func(context.Context) (Result, error)

type Observation struct {
	CheckedAt       time.Time
	NextCheckAt     time.Time
	Version         string
	Updated         bool
	Failure         string
	Failures        uint32
	BlockedReason   string `json:",omitempty"`
	RequiredVersion string `json:",omitempty"`
}

type Config struct {
	Check      Check
	Observe    func(Observation)
	Interval   time.Duration
	RetryFloor time.Duration
	RetryLimit time.Duration
	Now        func() time.Time
	Random     func(time.Duration) (time.Duration, error)
	// NextCheck can bring a calendar maintenance window forward without
	// removing ordinary availability checks or failure backoff.
	NextCheck func(time.Time, time.Time) time.Time
}

type Scheduler struct {
	config Config
	mu     sync.Mutex
	state  Observation
	wake   chan struct{}
}

func New(config Config) (*Scheduler, error) {
	if config.Check == nil {
		return nil, ErrInvalidConfig
	}
	if config.Interval == 0 {
		config.Interval = DefaultInterval
	}
	if config.RetryFloor == 0 {
		config.RetryFloor = DefaultRetryFloor
	}
	if config.RetryLimit == 0 {
		config.RetryLimit = DefaultRetryLimit
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = secureDuration
	}
	if config.Interval < time.Minute || config.RetryFloor <= 0 || config.RetryFloor > config.RetryLimit || config.RetryLimit > config.Interval {
		return nil, ErrInvalidConfig
	}
	return &Scheduler{config: config, wake: make(chan struct{}, 1)}, nil
}

// Wake applies changed machine preferences without restarting the updater.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) Snapshot() Observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// SeedBlockedActiveTerminalSessions restores a durable expected admission
// result before Run starts, preventing a restarted updater from immediately
// retrying the same activation through a recovery helper.
func (s *Scheduler) SeedBlockedActiveTerminalSessions(requiredVersion string, nextCheckAt time.Time) error {
	if requiredVersion == "" || nextCheckAt.IsZero() {
		return ErrInvalidConfig
	}
	s.mu.Lock()
	s.state.BlockedReason = BlockedActiveTerminalSessions
	s.state.RequiredVersion = requiredVersion
	s.state.NextCheckAt = nextCheckAt.UTC()
	s.state.Failure = ""
	s.state.Failures = 0
	s.mu.Unlock()
	return nil
}

func (s *Scheduler) Run(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidConfig
	}
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	failedEpisode := false
	for {
		next := s.Snapshot().NextCheckAt
		delay := next.Sub(s.config.Now())
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return updateContextFailure(ctx)
		case <-timer.C:
		case <-s.wake:
			timer.Stop()
		}
		err := s.runOnce(ctx)
		active, maintenance := expectedUpdateBlock(err)
		quiet := active != nil || maintenance != nil || updateCausesOnly(err, context.Canceled)
		if err != nil && !quiet && !failedEpisode {
			if !errorreport.HTTPAttemptObserved(err) {
				errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "update", "command", "command_failed", err)
			}
			failedEpisode = true
		}
		if err == nil && failedEpisode {
			errorreport.Current().Lifecycle(ctx, "update", "update", "recovered", "success")
			if recorder := diagnostics.FromContext(ctx); recorder != nil {
				if recordErr := recorder.RecordWithSupportReference("runtime", "recovered", "info", supportref.FromContext(ctx), map[string]string{"operation": "update", "outcome": "success"}); recordErr != nil {
					errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "diagnostic", "diagnostic_storage", "diagnostic_storage_unavailable", recordErr)
				}
			}
			failedEpisode = false
		}
		if ctx.Err() != nil {
			if err == nil {
				return updateContextFailure(ctx)
			}
			contextErr := updateContextFailure(ctx)
			if updateCausesOnly(err, ctx.Err()) && updateCausesOnly(contextErr, ctx.Err()) {
				return err
			}
			return updateFailure("automatic update check was interrupted", err, contextErr)
		}
	}
}

func (s *Scheduler) CheckNow(ctx context.Context) (Result, error) {
	return s.runCheck(ctx, s.config.Check)
}

func (s *Scheduler) runOnce(ctx context.Context) error {
	_, err := s.runCheck(ctx, s.config.Check)
	return err
}

// ObserveCheck records a caller-supplied check using the scheduler's normal
// success and retry bookkeeping without invoking the automatic activation
// callback. It is used by read-only control operations such as `pb update
// check`.
func (s *Scheduler) ObserveCheck(ctx context.Context, check Check) (Result, error) {
	if check == nil {
		return Result{}, ErrInvalidConfig
	}
	return s.runCheck(ctx, check)
}

func (s *Scheduler) runCheck(ctx context.Context, check Check) (Result, error) {
	checkedAt := s.config.Now().UTC()
	result, err := check(ctx)
	s.mu.Lock()
	state := s.state
	state.CheckedAt = checkedAt
	state.Version, state.Updated = result.Version, result.Updated
	pending := state.BlockedReason == BlockedActiveTerminalSessions && state.RequiredVersion != "" && (err != nil || !result.Updated)
	if !pending {
		state.BlockedReason, state.RequiredVersion = "", ""
	}
	active, maintenance := expectedUpdateBlock(err)
	if maintenance != nil {
		state.Failure, state.Failures = "", 0
		state.BlockedReason, state.RequiredVersion = BlockedOwnerMaintenance, maintenance.Notice.Version
		state.NextCheckAt = checkedAt.Add(s.config.RetryFloor)
		if maintenance.Notice.Deadline.After(checkedAt) && maintenance.Notice.Deadline.Before(state.NextCheckAt) {
			state.NextCheckAt = maintenance.Notice.Deadline
		}
	} else if active != nil {
		state.Failure, state.Failures = "", 0
		state.BlockedReason, state.RequiredVersion = BlockedActiveTerminalSessions, active.RequiredVersion
		state.NextCheckAt = checkedAt.Add(s.config.RetryFloor)
	} else if err == nil {
		state.Failure, state.Failures = "", 0
		if pending {
			state.NextCheckAt = checkedAt.Add(s.config.RetryFloor)
		} else {
			state.NextCheckAt = checkedAt.Add(s.jitteredInterval(ctx))
		}
	} else {
		if state.Failures < ^uint32(0) {
			state.Failures++
		}
		state.Failure = "check_failed"
		state.NextCheckAt = checkedAt.Add(s.retryDelay(state.Failures))
	}
	if s.config.NextCheck != nil {
		if next := s.config.NextCheck(checkedAt, state.NextCheckAt); next.After(checkedAt) && next.Before(state.NextCheckAt) {
			state.NextCheckAt = next
		}
	}
	s.state = state
	s.mu.Unlock()
	if s.config.Observe != nil {
		s.config.Observe(state)
	}
	return result, err
}

func (s *Scheduler) jitteredInterval(ctx context.Context) time.Duration {
	half := s.config.Interval / 2
	jitter, err := s.config.Random(s.config.Interval)
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "update", "command", "command_failed", err)
		return s.config.Interval
	}
	if jitter < 0 || jitter >= s.config.Interval {
		return s.config.Interval
	}
	return half + jitter
}

func (s *Scheduler) retryDelay(failures uint32) time.Duration {
	delay := s.config.RetryFloor
	for attempt := uint32(1); attempt < failures && delay < s.config.RetryLimit; attempt++ {
		if delay > s.config.RetryLimit/2 {
			return s.config.RetryLimit
		}
		delay *= 2
	}
	if delay > s.config.RetryLimit {
		return s.config.RetryLimit
	}
	return delay
}

func secureDuration(limit time.Duration) (time.Duration, error) {
	if limit <= 0 {
		return 0, ErrInvalidConfig
	}
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return 0, err
	}
	return time.Duration(binary.LittleEndian.Uint64(value[:]) % uint64(limit)), nil
}
