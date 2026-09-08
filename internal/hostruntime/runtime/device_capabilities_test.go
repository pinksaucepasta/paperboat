//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeviceCapabilityControllerClosesAdmissionBeforeAcknowledging(t *testing.T) {
	controller := newDeviceCapabilityController(nil)
	if controller.Enabled("terminal.v1") {
		t.Fatal("terminal admission must remain closed before authenticated policy")
	}
	enabled := deviceCapabilitySelection{Terminal: true, ManagedSSH: true, FileReceive: true, PreviewTunnel: true}
	if err := controller.Apply(context.Background(), deviceCapabilityPolicy{Schema: deviceCapabilitiesSchemaV1, Desired: enabled, DesiredVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if !controller.Enabled("ssh.v1") || controller.Enabled("peer-relay.v1") {
		t.Fatal("applied policy did not gate capabilities")
	}

	cleanupStarted := false
	controller.SetReconciler(func(_ context.Context, previous, next deviceCapabilitySelection) error {
		cleanupStarted = true
		if controller.Enabled("terminal.v1") {
			t.Error("new terminal admission remained open during disable cleanup")
		}
		if !previous.Terminal || next.Terminal {
			t.Error("reconciler received incorrect transition")
		}
		return nil
	})
	if err := controller.Apply(context.Background(), deviceCapabilityPolicy{Schema: deviceCapabilitiesSchemaV1, DesiredVersion: 2}); err != nil {
		t.Fatal(err)
	}
	if !cleanupStarted {
		t.Fatal("disable did not reconcile active resources")
	}
	observation := controller.Observation(time.Unix(10, 0))
	if observation.Status != "applied" || observation.Version != 2 || observation.Applied.Terminal {
		t.Fatalf("unexpected observation: %#v", observation)
	}
}

func TestDeviceCapabilityControllerReportsCleanupFailureAndRejectsStalePolicy(t *testing.T) {
	controller := newDeviceCapabilityController(nil)
	enabled := deviceCapabilitySelection{Terminal: true}
	if err := controller.Apply(context.Background(), deviceCapabilityPolicy{Schema: deviceCapabilitiesSchemaV1, Desired: enabled, DesiredVersion: 2}); err != nil {
		t.Fatal(err)
	}
	controller.SetReconciler(func(context.Context, deviceCapabilitySelection, deviceCapabilitySelection) error {
		return errors.New("cleanup failed")
	})
	if err := controller.Apply(context.Background(), deviceCapabilityPolicy{Schema: deviceCapabilitiesSchemaV1, DesiredVersion: 3}); err == nil {
		t.Fatal("expected cleanup failure")
	}
	observation := controller.Observation(time.Now())
	if observation.Status != "error" || observation.ErrorCode != "capability_cleanup_failed" || !observation.Applied.Terminal {
		t.Fatalf("failed cleanup was incorrectly acknowledged: %#v", observation)
	}
	if err := controller.Apply(context.Background(), deviceCapabilityPolicy{Schema: deviceCapabilitiesSchemaV1, Desired: enabled, DesiredVersion: 2}); err == nil {
		t.Fatal("expected stale policy rejection")
	}
}
