//go:build darwin || linux || windows

package runtime

import (
	"testing"
	"time"
)

func TestLazyRuntimeObservationIsStableAndInstallationFenced(t *testing.T) {
	started := time.Date(2099, 1, 2, 3, 4, 5, 0, time.FixedZone("offset", 3600))
	sender := &runtimeObservationSender{setupMode: "host", lazyBootID: "0123456789abcdef0123456789abcdef", lazyStartedAt: started, installationGeneration: 7}
	first := sender.lazyRuntimeObservation()
	second := sender.lazyRuntimeObservation()
	if first == nil || second == nil || first.Schema != "paperboat.lazy-runtime/v1" || first.BootID != sender.lazyBootID || first.InstallationGeneration != 7 || !first.StartedAt.Equal(started.UTC()) || *first != *second {
		t.Fatalf("lazy runtime observation is not stable: first=%+v second=%+v", first, second)
	}
	if got := (&runtimeObservationSender{setupMode: "host", lazyBootID: sender.lazyBootID, lazyStartedAt: started}).lazyRuntimeObservation(); got != nil {
		t.Fatalf("unfenced observation = %+v", got)
	}
}

func TestClientRuntimeDoesNotRegisterHostLazyRuntime(t *testing.T) {
	sender := &runtimeObservationSender{setupMode: "client", lazyBootID: "0123456789abcdef0123456789abcdef", lazyStartedAt: time.Now().UTC(), installationGeneration: 7}
	if got := sender.lazyRuntimeObservation(); got != nil {
		t.Fatalf("client advertised host lazy registration: %+v", got)
	}
	sender.setupMode = "host"
	if sender.lazyRuntimeObservation() == nil {
		t.Fatal("host registration lost")
	}
}
