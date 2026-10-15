package admission

import (
	"context"
	"sync"
	"time"
)

// Staging bounds connection setup by its caller and admission deadline. Accept
// hands the same context to the connection owner and removes those setup bounds.
type Staging struct {
	mu         sync.Mutex
	parent     context.Context
	deadline   time.Time
	lease      *Lease
	accepted   bool
	stopCaller func() bool
	timer      *time.Timer
}

func Stage(parent context.Context, deadline time.Time) (*Staging, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	lease, err := Admit(context.WithoutCancel(parent), deadline, nil)
	if err != nil {
		return nil, err
	}
	stage := &Staging{parent: parent, deadline: deadline, lease: lease}
	stage.stopCaller = context.AfterFunc(parent, func() {
		stage.mu.Lock()
		defer stage.mu.Unlock()
		if !stage.accepted {
			lease.cancel(context.Cause(parent))
		}
	})
	if !deadline.IsZero() {
		stage.timer = time.AfterFunc(time.Until(deadline), func() {
			stage.mu.Lock()
			defer stage.mu.Unlock()
			if !stage.accepted {
				lease.cancel(context.DeadlineExceeded)
			}
		})
	}
	return stage, nil
}
func (s *Staging) Context() context.Context { return s.lease.Context() }
func (s *Staging) Accept() (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopCaller()
	if s.timer != nil {
		s.timer.Stop()
	}
	err := s.parent.Err()
	if err == nil {
		err = s.lease.Check()
	}
	if err == nil && !s.deadline.IsZero() && !time.Now().Before(s.deadline) {
		err = ErrExpired
	}
	if err != nil {
		s.lease.cancel(err)
		return nil, err
	}
	s.accepted = true
	return s.lease, nil
}
func (s *Staging) Abort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopCaller()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.lease.Close()
}
