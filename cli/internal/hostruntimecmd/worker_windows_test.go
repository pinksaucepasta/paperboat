//go:build windows

package hostruntimecmd

import (
	"bytes"
	"context"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func TestWindowsWorkerRejectsNonPipeEndpoint(t *testing.T) {
	err := runWorker(context.Background(), []string{
		"--socket", `C:\\unsafe.sock`, "--token-file", `C:\\token`, "--owner-sid", "S-1-5-21-1", "--worker-id", "runtime-test",
	}, strings.NewReader(""), nilWriter{}, nilWriter{})
	if err == nil || !strings.Contains(err.Error(), "invalid worker invocation") {
		t.Fatalf("err = %v, want invalid Windows worker invocation", err)
	}
}

func TestWindowsHostdWorkerEnvironmentCarriesInstalledMachine(t *testing.T) {
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		t.Fatal(err)
	}
	{
		install := hostinstall.WindowsRuntimeConfig{OwnerSID: "S-1-5-21-1", TokenFile: hostinstall.WindowsHostdTokenPath(), StateRoot: `C:\State`, Workspace: `C:\Workspace`, ControlURL: "https://api.pprbt.dev", ListenAddress: "127.0.0.1:8080", MachineID: "machine"}
		environment, err := windowsHostdWorkerEnvironment(install, layout, `C:\Program Files\Paperboat\runtime.exe`)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := environment["PAPERBOAT_SETUP_MODE"]; exists {
			t.Fatal("retired setup mode injected")
		}
		systemDirectory, err := windows.GetSystemDirectory()
		if err != nil {
			t.Fatal(err)
		}
		wantShell := filepath.Join(systemDirectory, "cmd.exe")
		if environment["PAPERBOAT_SHELL"] != wantShell {
			t.Fatalf("shell=%q, want %q", environment["PAPERBOAT_SHELL"], wantShell)
		}
	}
}

func TestParseWindowsWorkerStatus(t *testing.T) {
	status, err := parseWindowsWorkerStatus("active 7 1\n", "active", "runtime-test")
	if err != nil {
		t.Fatal(err)
	}
	if status != (hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime-test", Epoch: 7, APIVersion: 1}) {
		t.Fatalf("status = %+v", status)
	}
	if _, err := parseWindowsWorkerStatus("active 0 1\n", "active", "runtime-test"); err == nil {
		t.Fatal("zero lifecycle epoch was accepted")
	}
}

type nilWriter struct{}

func (nilWriter) Write(value []byte) (int, error) { return len(value), nil }

func workerStartupFixture(t *testing.T) (*hostdproto.Server, string, []byte, []string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if user.User.Sid.String() != "S-1-5-18" {
		t.Skip("native worker startup capability fixture requires SYSTEM, matching installed token ownership")
	}
	root := t.TempDir()
	endpoint := fmt.Sprintf(`\\.\pipe\PaperboatWorkerStartup-%d-%d`, os.Getpid(), time.Now().UnixNano())
	token := bytes.Repeat([]byte{0x56}, 32)
	tokenPath := filepath.Join(root, "token")
	const ownerSID = "S-1-5-21-111-222-333-1001"
	if err := atomicfile.Write(tokenPath, token, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + ownerSID + ")"}); err != nil {
		t.Fatal(err)
	}
	// Match an enrolled user SID while the native owner runs as SYSTEM.
	stateRoot := filepath.Join(root, "state")
	if err := os.Mkdir(stateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:" + ownerSID + "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + ownerSID + ")")
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := absolute.Owner()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := absolute.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(stateRoot, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
	}); err != nil {
		t.Fatal(err)
	}
	server, err := hostdproto.NewServer(hostdproto.SocketConfig{SocketPath: endpoint, StatePath: filepath.Join(stateRoot, "fence.json"), SID: ownerSID, Token: token, APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("listener cleanup: %v", err)
		}
	})
	return server, endpoint, token, []string{"--socket", endpoint, "--token-file", tokenPath, "--owner-sid", ownerSID}
}
