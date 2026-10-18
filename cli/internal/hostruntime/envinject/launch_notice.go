package envinject

import (
	"context"
	"errors"
	"sync"
)

// LaunchNoticePending says managed values were not applied to a launch because
// their delivery is pending or unavailable. The launch itself still runs.
const LaunchNoticePending = "environment_pending"

type launchNoticeKey struct{}

// LaunchNotices collects what a launch learned about managed ENV so the caller
// can tell the user once, without failing the launch.
type LaunchNotices struct {
	mu    sync.Mutex
	codes []string
}

// WithLaunchNotices attaches a collector to one launch operation.
func WithLaunchNotices(ctx context.Context) (context.Context, *LaunchNotices) {
	notices := &LaunchNotices{}
	return context.WithValue(ctx, launchNoticeKey{}, notices), notices
}

// NoteLaunch records a notice on the launch's collector when it has one.
func NoteLaunch(ctx context.Context, code string) {
	notices, _ := ctx.Value(launchNoticeKey{}).(*LaunchNotices)
	if notices == nil {
		return
	}
	notices.mu.Lock()
	defer notices.mu.Unlock()
	for _, existing := range notices.codes {
		if existing == code {
			return
		}
	}
	notices.codes = append(notices.codes, code)
}

// Codes returns the recorded notice codes in the order they were noted.
func (n *LaunchNotices) Codes() []string {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.codes...)
}

// Undeliverable reports whether err only says managed ENV could not be delivered.
func Undeliverable(err error) bool {
	var delivery *DeliveryError
	return errors.As(err, &delivery) || errors.Is(err, ErrNotReady)
}
