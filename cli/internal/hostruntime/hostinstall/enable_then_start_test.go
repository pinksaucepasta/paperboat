package hostinstall

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

type recordingNative struct {
	service.NativeLifecycleController
	calls []string
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
