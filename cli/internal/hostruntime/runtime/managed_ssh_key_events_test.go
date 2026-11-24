//go:build darwin || linux || windows

package runtime

import (
	"errors"
	"net"
	goruntime "runtime"
	"testing"
	"time"

	runtimeidentity "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents/eventstest"
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
			_, _, err := newProductionManagedSSH("https://example.test", nil, registration, func() (runtimeidentity.Registration, error) { return registration, nil }, revocationTestAuth{}, 1, 0, newMachineCapabilityController(nil), tc.events, nil)
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
	deadline := time.Now().Add(5 * time.Second)
	for !subscriptions[0].Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("machine access subscription did not become healthy")
		}
		time.Sleep(time.Millisecond)
	}
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

// A port saved while the worker runs takes effect without a restart: a worker that started on a port nobody listens on
// used to keep it, and every SSH stream then failed until the next restart.
func TestManagedSSHFollowsASavedPort(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows serves the port of its installed sshd generation")
	}
	listen := func() (net.Listener, uint16) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return l, uint16(l.Addr().(*net.TCPAddr).Port)
	}
	first, firstPort := listen()
	second, secondPort := listen()
	defer second.Close()
	_ = first.Close() // nothing answers on the port the worker starts with
	saved := runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 1, SSHUser: "owner", SSHPort: firstPort}
	registration := saved
	_, subscriptions, _ := eventstest.Start(t, time.Hour, machineevents.TopicMachineAccess)
	host, service, err := newProductionManagedSSH("https://example.test", nil, registration, func() (runtimeidentity.Registration, error) { return saved, nil }, revocationTestAuth{}, 1, 7, newMachineCapabilityController(nil), subscriptions[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	managed := service.(*managedSSHService)
	_, _ = managed.initialize(t.Context())
	if _, ok := host.Target(); ok {
		t.Fatal("a target was published for a port nobody listens on")
	}
	saved.SSHPort = secondPort
	_, _ = managed.initialize(t.Context()) // later steps need a real account; the target is published first
	if target, ok := host.Target(); !ok || target.Port != secondPort {
		t.Fatalf("target after the saved port changed = %+v, %v", target, ok)
	}
	third, thirdPort := listen()
	defer third.Close()
	saved.SSHPort = thirdPort
	managed.retarget(t.Context())
	if target, _ := host.Target(); target.Port != thirdPort || target.Generation != 7 {
		t.Fatalf("running target did not follow the saved port: %+v", target)
	}
	saved.SSHPort = firstPort
	managed.retarget(t.Context())
	if target, ok := host.Target(); !ok || target.Port != thirdPort {
		t.Fatalf("an unusable saved port took SSH down: %+v, %v", target, ok)
	}
}
