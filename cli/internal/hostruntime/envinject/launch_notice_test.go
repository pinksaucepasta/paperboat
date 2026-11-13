package envinject

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestLaunchNoticeForNamesWhyDeliveryFailed(t *testing.T) {
	timeout := &net.DNSError{IsTimeout: true}
	for name, test := range map[string]struct {
		err  error
		want string
	}{
		"not ready":         {&DeliveryError{Cause: ErrNotReady}, LaunchNoticePending},
		"paperboat timeout": {&DeliveryError{Cause: fmt.Errorf("register: %w", timeout)}, LaunchNoticeOffline},
		"request deadline":  {&DeliveryError{Cause: context.DeadlineExceeded}, LaunchNoticeOffline},
		"delivery refused":  {&DeliveryError{Cause: errors.New("refused")}, LaunchNoticeUndeliverable},
	} {
		if got := LaunchNoticeFor(test.err); got != test.want {
			t.Fatalf("%s: notice=%q want %q", name, got, test.want)
		}
	}
}
