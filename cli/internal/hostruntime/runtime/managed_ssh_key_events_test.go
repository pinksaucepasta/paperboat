//go:build darwin || linux || windows

package runtime

import (
	"errors"
	"testing"
	"time"

	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
)

func TestManagedSSHRequiresMachineAccessSubscription(t *testing.T) {
	registration := runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 1, SSHUser: "owner", SSHPort: 22}
	_, subscriptions, _ := eventstest.Start(t, time.Hour, machineevents.TopicMachineAccess)
	for _, tc := range []struct {
		name      string
		events    *machineevents.Subscription
		wantError bool
	}{{"missing", nil, true}, {"subscribed", subscriptions[0], false}} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := newProductionManagedSSH("https://example.test", nil, registration, revocationTestAuth{}, 1, 0, newMachineCapabilityController(nil), tc.events, nil)
			if tc.wantError {
				if !errors.Is(err, ErrProductionInvalid) {
					t.Fatalf("missing subscription error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The shared scheduler serves Windows as well as Unix. Healthy delivery suppresses
// unchanged-key ticks, but cannot postpone an authority refresh past five minutes.
func TestManagedSSHKeysSafetyRefreshAfterFiveMinutes(t *testing.T) {
	_, subscriptions, _ := eventstest.Start(t, time.Hour, machineevents.TopicMachineAccess)
	service := &managedSSHKeyReconciler{events: subscriptions[0], publicKeys: []string{"unchanged"}, readPublicKeys: func() ([]string, error) { return []string{"unchanged"}, nil }}
	last := time.Now()
	for _, tc := range []struct {
		name                               string
		elapsed                            time.Duration
		failed, published, signalled, want bool
	}{
		{name: "unchanged_tick", elapsed: 30 * time.Second},
		{name: "unchanged_file_event", elapsed: time.Minute, signalled: true},
		{name: "before_deadline", elapsed: 5*time.Minute - time.Nanosecond},
		{name: "safety_deadline", elapsed: 5 * time.Minute, want: true},
		{name: "published", elapsed: time.Second, published: true, want: true},
		{name: "retry", elapsed: 30 * time.Second, failed: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := service.reconcileDue(last.Add(tc.elapsed), last, tc.failed, tc.published, tc.signalled); got != tc.want {
				t.Fatalf("refresh due = %t, want %t", got, tc.want)
			}
		})
	}
}
