package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// killingController is a service manager whose stop ends the calling process, as `systemctl stop` / `launchctl
// bootout` of the caller's own unit does: it records what was still on disk at that moment, and nothing after the
// stop runs.
type killingController struct {
	definition string
	calls      []string
	leftAtStop bool
}

var errKilled = errors.New("the service manager stopped the calling process")

func (k *killingController) record(call string) { k.calls = append(k.calls, call) }
func (k *killingController) Apply(context.Context, string, bool) error {
	k.record("apply")
	return nil
}
func (k *killingController) Remove(context.Context, string) error {
	k.record("remove")
	return k.stop()
}
func (k *killingController) Inspect(context.Context, string) (NativeControllerStatus, error) {
	return NativeControllerStatus{}, nil
}
func (k *killingController) Enable(context.Context, string) error { k.record("enable"); return nil }
func (k *killingController) Disable(context.Context, string) error {
	k.record("disable")
	if _, err := os.Lstat(k.definition); err != nil {
		return errors.New("disable needs the unit file to drop its boot links")
	}
	return nil
}
func (k *killingController) Start(context.Context, string) error { k.record("start"); return nil }
func (k *killingController) Stop(context.Context, string) error  { k.record("stop"); return k.stop() }
func (k *killingController) stop() error {
	_, err := os.Lstat(k.definition)
	k.leftAtStop = err == nil
	return errKilled
}

// The updater removes its own unit last (an interrupted install had installed it on a machine without one). The
// stop kills the updater, so a removal that stops first (Uninstall: `disable --now`, then delete) left the unit file
// or plist behind forever. RemoveSelf deletes the declaration before the stop.
func TestRemoveSelfDeletesTheDeclarationBeforeTheStopThatEndsTheCaller(t *testing.T) {
	for _, test := range []struct {
		platform string
		want     []string
	}{
		{"linux", []string{"disable", "stop"}},
		{"darwin", []string{"stop"}},
	} {
		definition := filepath.Join(t.TempDir(), "paperboat-updated.unit")
		if err := os.WriteFile(definition, []byte("declaration"), 0o644); err != nil {
			t.Fatal(err)
		}
		controller := &killingController{definition: definition}
		component := &NativeTransactionalComponent{installer: &Installer{config: Config{Platform: test.platform}, definitionPath: definition}, controller: controller, id: UpdaterKind}
		if err := component.RemoveSelf(context.Background()); !errors.Is(err, errKilled) {
			t.Fatalf("%s: %v", test.platform, err)
		}
		if controller.leftAtStop {
			t.Fatalf("%s: the declaration was still on disk when the stop ended the caller", test.platform)
		}
		if !reflect.DeepEqual(controller.calls, test.want) {
			t.Fatalf("%s: calls=%v want %v", test.platform, controller.calls, test.want)
		}
	}
}

type selfRemovingController struct {
	killingController
	removedSelf string
}

func (c *selfRemovingController) RemoveSelf(_ context.Context, definitionPath string) error {
	c.removedSelf = definitionPath
	return nil
}

// Windows removes a running service from inside it in one service-manager step (mark for deletion, declaration,
// stop): the component hands the whole removal to a manager that has one, and disables or stops nothing itself.
func TestRemoveSelfUsesTheServiceManagersOwnSelfRemoval(t *testing.T) {
	definition := filepath.Join(t.TempDir(), "PaperboatCarrier-u1.json")
	controller := &selfRemovingController{}
	component := &NativeTransactionalComponent{installer: &Installer{config: Config{Platform: "windows"}, definitionPath: definition}, controller: controller, id: CarrierKind}
	if err := component.RemoveSelf(context.Background()); err != nil || controller.removedSelf != definition || len(controller.calls) != 0 {
		t.Fatalf("err=%v removedSelf=%q calls=%v", err, controller.removedSelf, controller.calls)
	}
}

type selfRemovingFake struct {
	*fakeTransactionalComponent
	removed bool
}

func (f *selfRemovingFake) RemoveSelf(context.Context) error { f.removed = true; return nil }

// The rollback hands the caller its own removal as RemoveSelf, not as a Restore that stops before it deletes.
func TestRecoverWithinHandsTheCallerRemoveSelf(t *testing.T) {
	updater := &selfRemovingFake{fakeTransactionalComponent: newFakeTransactionalComponent("updater", nil, nil)}
	manager := lifecycleForTest(t, newFakeTransactionalComponent("hostd", nil, nil), updater, killedOnInstall{newFakeTransactionalComponent("host", nil, nil)})
	func() {
		defer func() { _ = recover() }()
		_ = manager.Install(context.Background())
	}()
	finishSelf, err := manager.RecoverWithin(context.Background(), "updater")
	if err != nil || finishSelf == nil {
		t.Fatalf("finishSelf=%v err=%v", finishSelf != nil, err)
	}
	if err := finishSelf(context.Background()); err != nil || !updater.removed {
		t.Fatalf("removed=%v err=%v", updater.removed, err)
	}
}
