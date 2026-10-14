//go:build windows

package windowsopenssh

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
	"os"
	"reflect"
	"testing"
)

func TestNativeStoppedSSHRegistrationRollback(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("native elevated Windows fixture required")
	}
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	owner := fmt.Sprintf("S-1-5-21-102938-475869-192837-%d", binary.LittleEndian.Uint32(nonce[:])|1024)
	instance, err := hostservice.WindowsUserInstance(owner)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(nil)
	config.OwnerSID = owner
	config.ServiceName = ServiceName + "-" + instance
	config.StateRoot = t.TempDir()
	config.ServiceExecutable, err = os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Disconnect()
	s, err := m.CreateService(config.ServiceName, config.ServiceExecutable, mgr.Config{StartType: mgr.StartManual, ErrorControl: mgr.ErrorNormal, ServiceStartName: "LocalSystem", SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED}, "daemon", "__windows-sshd-service", "--instance", instance)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	defer func() {
		if s, e := m.OpenService(config.ServiceName); e == nil {
			s.Delete()
			s.Close()
		}
	}()
	ctx := context.Background()
	before, err := CaptureService(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Present || before.Running {
		t.Fatal("fixture must be stopped")
	}
	if err := RemoveServiceOwned(ctx, config); err != nil {
		t.Fatal(err)
	}
	if err := RestoreService(ctx, config, *before); err != nil {
		t.Fatal(err)
	}
	after, err := CaptureService(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("native rollback changed stopped registration configuration or recovery")
	}
	bad := *before
	bad.Configuration.BinaryPathName = windows.ComposeCommandLine([]string{config.ServiceExecutable, "daemon", "__windows-sshd-service", "--instance", "uforeign"})
	if err := RestoreService(ctx, config, bad); err == nil {
		t.Fatal("accepted foreign snapshot target")
	}
	after, err = CaptureService(ctx, config)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("rejected snapshot changed native registration")
	}
}
