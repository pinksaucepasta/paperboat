//go:build windows

package updated

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

type noopWindowsActivationGate struct{}

func (noopWindowsActivationGate) Candidate(context.Context, workerupdate.GateRequest) error {
	return nil
}
func (noopWindowsActivationGate) Drain(context.Context, workerupdate.GateRequest) error  { return nil }
func (noopWindowsActivationGate) Active(context.Context, workerupdate.GateRequest) error { return nil }
func (noopWindowsActivationGate) Commit(context.Context, workerupdate.GateRequest) error {
	return nil
}
func (noopWindowsActivationGate) Rollback(context.Context, workerupdate.GateRequest) error {
	return nil
}

func TestWaitForWindowsUpdaterVersionWaitsForApplicationReadiness(t *testing.T) {
	calls := 0
	err := waitForWindowsUpdaterVersion(context.Background(), "2026.08.28.2", time.Second, time.Millisecond, func(context.Context) (ControlResponse, error) {
		calls++
		if calls < 3 {
			return ControlResponse{Version: "2026.08.28.1"}, nil
		}
		return ControlResponse{Version: "2026.08.28.2"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("status calls=%d want=3", calls)
	}
}

func TestWaitForWindowsUpdaterVersionReturnsExactMismatch(t *testing.T) {
	err := waitForWindowsUpdaterVersion(context.Background(), "2026.08.28.2", 5*time.Millisecond, time.Millisecond, func(context.Context) (ControlResponse, error) {
		return ControlResponse{Version: "2026.08.28.1"}, nil
	})
	if !errors.Is(err, errInvalidWindowsActivation) || !strings.Contains(err.Error(), `got "2026.08.28.1", want "2026.08.28.2"`) {
		t.Fatalf("error=%v", err)
	}
}

func TestWaitForWindowsDaemonVersionWaitsForExactCandidate(t *testing.T) {
	versions := []string{"2026.08.28.1", "2026.08.28.2"}
	calls := 0
	err := waitForWindowsDaemonVersion(context.Background(), "2026.08.28.2", time.Second, time.Millisecond, func(context.Context) (localapi.Snapshot, error) {
		version := versions[calls]
		calls++
		return localapi.Snapshot{DaemonVersion: version}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("snapshot calls=%d want=2", calls)
	}
}

func TestWaitForWindowsDaemonVersionReturnsExactMismatch(t *testing.T) {
	err := waitForWindowsDaemonVersion(context.Background(), "2026.08.28.2", 5*time.Millisecond, time.Millisecond, func(context.Context) (localapi.Snapshot, error) {
		return localapi.Snapshot{DaemonVersion: "2026.08.28.1"}, nil
	})
	if !errors.Is(err, errInvalidWindowsActivation) || !strings.Contains(err.Error(), `got "2026.08.28.1", want "2026.08.28.2"`) {
		t.Fatalf("error=%v", err)
	}
}

func testWindowsUpdaterConfig(t *testing.T) WindowsConfig {
	t.Helper()
	layout, err := service.WindowsUserLayout("S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	instanceRoot, _ := hostinstall.WindowsInstanceRoot(layout.Instance)
	return WindowsConfig{StateRoot: layout.UpdateStateRoot, RuntimeStateRoot: `C:\Users\Pujan\AppData\Local\Paperboat\runtime`, Binary: layout.Binary, BinaryRollback: layout.BinaryRollback, BinaryStaged: layout.BinaryStaged, OwnerSID: "S-1-5-21-1-2-3-1001", MachineID: "machine", RepositoryURL: "https://get.pprbt.dev", TokenFile: filepath.Join(instanceRoot, "hostd.token"), InstallState: filepath.Join(instanceRoot, "runtime-install.json"), ControlSocket: `\\.\pipe\PaperboatUpdatedControl-` + layout.Instance, HostdSocket: layout.HostdSocket, HealthURL: "http://127.0.0.1:8080/healthz", ActiveVersion: "2026.08.23.1", Architecture: "amd64", SetupMode: "client", ActivationGate: noopWindowsActivationGate{}, CandidateStarter: func(context.Context, workerupdate.StartRequest) (workerupdate.Worker, error) {
		return nil, errors.New("candidate test stub")
	}}
}

func TestWindowsUpdaterRejectsMutableTrustAndPathInputs(t *testing.T) {
	baseline := testWindowsUpdaterConfig(t)
	if !validWindowsConfig(baseline) {
		t.Fatal("valid fixed updater config rejected")
	}
	tests := []func(*WindowsConfig){
		func(c *WindowsConfig) { c.TokenFile = `C:\Temp\token` },
		func(c *WindowsConfig) { c.InstallState = `C:\Temp\state.json` },
		func(c *WindowsConfig) { c.StateRoot = `C:\Temp\updates` },
		func(c *WindowsConfig) { c.RuntimeStateRoot = `C:\Temp\owner-state` },
		func(c *WindowsConfig) { c.ControlSocket = `\\.\pipe\attacker` },
		func(c *WindowsConfig) { c.HealthURL = "http://10.0.0.1:8080/healthz" },
		func(c *WindowsConfig) { c.Architecture = "386" },
		func(c *WindowsConfig) { c.SetupMode = "both" },
	}
	for index, mutate := range tests {
		candidate := baseline
		mutate(&candidate)
		if validWindowsConfig(candidate) {
			t.Fatalf("mutable trust case %d accepted", index)
		}
	}
}

func TestWindowsUpdaterLocalDaemonReadyUsesInjectedProbe(t *testing.T) {
	config := testWindowsUpdaterConfig(t)
	calls := 0
	config.localDaemonReady = func() bool { calls++; return true }
	if !config.LocalDaemonReady() || calls != 1 {
		t.Fatalf("ready=%v calls=%d", config.LocalDaemonReady(), calls)
	}
}

func TestPrivilegedWindowsServiceIdentityContract(t *testing.T) {
	config := mgr.Config{ServiceStartName: "LocalSystem", StartType: mgr.StartAutomatic, ErrorControl: mgr.ErrorNormal, SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED}
	if !validPrivilegedWindowsServiceConfig(config, mgr.StartAutomatic, mgr.ErrorNormal) {
		t.Fatal("valid LocalSystem service identity rejected")
	}
	config.ServiceStartName = "Paperboat"
	if validPrivilegedWindowsServiceConfig(config, mgr.StartAutomatic, mgr.ErrorNormal) {
		t.Fatal("mutable service account accepted")
	}
	config.ServiceStartName = "LocalSystem"
	config.SidType = windows.SERVICE_SID_TYPE_NONE
	if validPrivilegedWindowsServiceConfig(config, mgr.StartAutomatic, mgr.ErrorNormal) {
		t.Fatal("mutable service SID type accepted")
	}
}

func TestWindowsRecoveryPolicyIsServiceSpecific(t *testing.T) {
	standard := windowsRecoveryActionsForService(windowsHostdService)
	if !windowsRecoveryActionsMatch(standard, []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 15 * time.Second}, {Type: mgr.ServiceRestart, Delay: time.Minute}}) {
		t.Fatal("hostd/updater recovery policy changed")
	}
	if !windowsRecoveryActionsMatch(windowsRecoveryActionsForService(windowsUpdaterService), standard) {
		t.Fatal("PaperboatUpdated does not use the standard recovery policy")
	}
	ssh := windowsRecoveryActionsForService(windowsSSHService)
	if !windowsRecoveryActionsMatch(ssh, []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 30 * time.Second}, {Type: mgr.NoAction}}) {
		t.Fatal("PaperboatSshd recovery policy changed")
	}
	if windowsRecoveryActionsMatch(standard, ssh) {
		t.Fatal("PaperboatSshd incorrectly shares the updater recovery policy")
	}
}

func TestWindowsSSHCommandContractIsExact(t *testing.T) {
	valid := []string{"daemon", "__windows-sshd-service", "--instance", "u0123456789abcdef01234567"}
	if !validWindowsSSHArguments(valid) {
		t.Fatal("valid fixed PaperboatSshd command rejected")
	}
	mutated := append([]string(nil), valid...)
	mutated[3] = "u1"
	if validWindowsSSHArguments(mutated) {
		t.Fatal("mutable sshd executable accepted")
	}
}

func TestWindowsActiveServiceTargetsUseCanonicalBinary(t *testing.T) {
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		t.Fatal(err)
	}
	hostd := windowsServiceTarget{Executable: layout.Binary}
	updater := windowsServiceTarget{Executable: layout.Binary}
	ssh := windowsServiceTarget{Executable: layout.Binary}
	if !activeWindowsServiceTargetsMatch(layout, "2026.08.23.1", hostd, updater, ssh) {
		t.Fatal("exact role targets rejected")
	}
	hostd.Executable = layout.BinaryRollback
	if activeWindowsServiceTargetsMatch(layout, "2026.08.23.1", hostd, updater, ssh) {
		t.Fatal("runtime artifact accepted as hostd")
	}
	hostd.Executable = layout.Binary
	updater.Executable = layout.BinaryRollback
	if !activeWindowsServiceTargetsMatch(layout, "2026.08.23.1", hostd, updater, ssh) {
		t.Fatal("intentional rollback artifact rejected as updater")
	}
	updater.Executable = `C:\Temp\pb.exe`
	if activeWindowsServiceTargetsMatch(layout, "2026.08.23.1", hostd, updater, ssh) {
		t.Fatal("mutable updater artifact accepted")
	}
}

func TestNormalizeWindowsRollbackTargetsRestartsUpdaterFromCanonicalPath(t *testing.T) {
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		t.Fatal(err)
	}
	hostd := windowsServiceTarget{Executable: layout.Binary, Arguments: []string{"daemon", "__runtime-hostd"}}
	updater := windowsServiceTarget{Executable: layout.BinaryRollback, Arguments: []string{"daemon", "__runtime-updated"}, WasRunning: true}
	ssh := windowsServiceTarget{}
	_, normalized, _, err := normalizeWindowsRollbackTargets(hostd, updater, ssh)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Executable != layout.Binary || !normalized.WasRunning {
		t.Fatalf("normalized updater=%+v want canonical executable %q", normalized, layout.Binary)
	}
}

func TestWindowsActivationPathsAcceptRollbackUpdaterDuringRecovery(t *testing.T) {
	config := testWindowsUpdaterConfig(t)
	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := canonicalWindowsRelease(layout, "2026.08.24.1")
	if err != nil {
		t.Fatal(err)
	}
	component := windowsActivationComponent{Path: paths.Runtime, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Length: 1}
	journal := windowsActivationJournal{
		Schema: windowsActivationJournalSchema, TransactionID: "0123456789abcdef0123456789abcdef",
		PreviousVersion: config.ActiveVersion, Version: "2026.08.24.1", Architecture: config.Architecture,
		Stage: windowsActivationStaged, Runtime: component, CLI: component, Hostd: component, Updater: component,
		PreviousBinary: windowsActivationComponent{Path: layout.Binary, SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Length: 1},
		OldHostd:       windowsServiceTarget{Executable: layout.Binary, Arguments: []string{"daemon", "__runtime-hostd", "--instance", layout.Instance}},
		NewHostd:       windowsServiceTarget{Executable: layout.Binary, Arguments: []string{"daemon", "__runtime-hostd", "--instance", layout.Instance}},
		OldUpdater:     windowsServiceTarget{Executable: layout.BinaryRollback, Arguments: []string{"daemon", "__runtime-updated", "--instance", layout.Instance}},
		NewUpdater:     windowsServiceTarget{Executable: layout.Binary, Arguments: []string{"daemon", "__runtime-updated", "--instance", layout.Instance}},
	}
	if !validWindowsActivationPaths(config, journal) {
		t.Fatal("staged journal with rollback updater was rejected")
	}
	journal.NewHostd.Arguments[3] = "u0123456789abcdef01234567"
	if validWindowsActivationPaths(config, journal) {
		t.Fatal("foreign instance accepted in owner transaction")
	}
	journal.NewHostd.Arguments[3] = layout.Instance
	journal.OldUpdater.Executable = `C:\Temp\pb.exe`
	if validWindowsActivationPaths(config, journal) {
		t.Fatal("mutable updater executable was accepted")
	}
}

func TestWindowsRuntimeServiceTargetPreservesExactInstance(t *testing.T) {
	const instance = "u0123456789abcdef01234567"
	executable := `C:\Program Files\Paperboat\users\` + instance + `\bin\pb.exe`
	for _, role := range []struct{ name, command string }{{windowsHostdService, "__runtime-hostd"}, {windowsUpdaterService, "__runtime-updated"}} {
		args := []string{executable, "daemon", role.command, "--instance", instance}
		name := role.name + "-" + instance
		target, err := parseWindowsRuntimeServiceTarget(name, role.command, windows.ComposeCommandLine(args))
		if err != nil || target.Executable != executable || strings.Join(target.Arguments, "|") != strings.Join(args[1:], "|") {
			t.Fatalf("per-user service target lost its binding: %+v, %v", target, err)
		}
		for _, wrong := range [][]string{
			args[:3], append(append([]string(nil), args...), "extra"),
			{executable, "daemon", role.command, "--instance", "u1123456789abcdef01234567"},
			{executable, "daemon", role.command, "--instance", "unot-a-valid-instance0000"},
			{executable, "daemon", "__runtime-other", "--instance", instance},
		} {
			if _, err := parseWindowsRuntimeServiceTarget(name, role.command, windows.ComposeCommandLine(wrong)); !errors.Is(err, errInvalidWindowsActivation) {
				t.Fatalf("accepted wrong instance/role arguments %q: %v", wrong, err)
			}
		}
		if _, err := parseWindowsRuntimeServiceTarget(role.name+"-u1123456789abcdef01234567", role.command, windows.ComposeCommandLine(args)); !errors.Is(err, errInvalidWindowsActivation) {
			t.Fatal("accepted another user's service name")
		}
	}
}

func TestWindowsInstanceServiceRecoveryPolicy(t *testing.T) {
	const instance = "u4c8e2991570c314b650297e5"
	ssh := windowsRecoveryActionsForService("PaperboatSshd-" + instance)
	if !windowsRecoveryActionsMatch(ssh, windowsopenssh.ServiceRecoveryActions()) {
		t.Fatalf("instance SSH must retain its bounded restart policy: %v", ssh)
	}
	for _, name := range []string{"PaperboatHostd-" + instance, "PaperboatUpdated-" + instance, "PaperboatSshd-" + instance + "-foreign"} {
		if !windowsRecoveryActionsMatch(windowsRecoveryActionsForService(name), standardWindowsRecoveryActions()) {
			t.Errorf("unexpected recovery policy for %q", name)
		}
	}
}
