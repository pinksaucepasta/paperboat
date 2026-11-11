package hostinstall

import (
	"context"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

type recordingNative struct {
	service.NativeLifecycleController
	calls  []string
	status service.NativeControllerStatus
}

func (r *recordingNative) Inspect(context.Context, string) (service.NativeControllerStatus, error) {
	return r.status, nil
}
func (r *recordingNative) Disable(context.Context, string) error {
	r.calls = append(r.calls, "disable")
	return nil
}
func (r *recordingNative) Stop(context.Context, string) error {
	r.calls = append(r.calls, "stop")
	return nil
}

func (r *recordingNative) Enable(context.Context, string) error {
	r.calls = append(r.calls, "enable")
	return nil
}
func (r *recordingNative) Start(context.Context, string) error {
	r.calls = append(r.calls, "start")
	return nil
}

// A retired (disabled) generation is enabled before it starts; an enabled one is just started.
func TestEnableThenStartEnablesOnlyADisabledService(t *testing.T) {
	disabled := &recordingNative{}
	if err := enableThenStart(context.Background(), disabled, "x.json", service.NativeControllerStatus{Registered: true}); err != nil {
		t.Fatal(err)
	}
	if got := disabled.calls; len(got) != 2 || got[0] != "enable" || got[1] != "start" {
		t.Fatalf("disabled service: %v, want enable then start", got)
	}
	enabled := &recordingNative{}
	if err := enableThenStart(context.Background(), enabled, "x.json", service.NativeControllerStatus{Registered: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := enabled.calls; len(got) != 1 || got[0] != "start" {
		t.Fatalf("enabled service: %v, want start only", got)
	}
}

// Retiring a carrier generation disables it before stopping it, so the service manager's restart recovery cannot bring
// the stopped process back (audit C #13); an already disabled or stopped one is not touched again.
func TestRetireNativeDisablesBeforeItStops(t *testing.T) {
	for _, test := range []struct {
		name   string
		status service.NativeControllerStatus
		want   string
	}{
		{"enabled and running", service.NativeControllerStatus{Registered: true, Enabled: true, Running: true}, "disable stop"},
		{"disabled but still running", service.NativeControllerStatus{Registered: true, Running: true}, "stop"},
		{"enabled and stopped", service.NativeControllerStatus{Registered: true, Enabled: true}, "disable"},
		{"disabled and stopped", service.NativeControllerStatus{Registered: true}, ""},
	} {
		native := &recordingNative{status: test.status}
		if _, err := retireNative(context.Background(), native, "x.json"); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if got := strings.Join(native.calls, " "); got != test.want {
			t.Errorf("%s: %q, want %q", test.name, got, test.want)
		}
	}
}
