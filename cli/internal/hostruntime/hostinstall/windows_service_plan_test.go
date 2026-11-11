package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

type fakeWindowsServicePlanStep struct {
	path         string
	events       *[]string
	installErr   error
	startErr     error
	uninstallErr error
}

func (step *fakeWindowsServicePlanStep) DefinitionPath() string { return step.path }

func (step *fakeWindowsServicePlanStep) Install(context.Context) error {
	*step.events = append(*step.events, "install:"+step.path)
	return step.installErr
}

func (step *fakeWindowsServicePlanStep) Start(context.Context) error {
	*step.events = append(*step.events, "start:"+step.path)
	return step.startErr
}

func (step *fakeWindowsServicePlanStep) Uninstall(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("rollback context was not live")
	}
	*step.events = append(*step.events, "uninstall:"+step.path)
	return step.uninstallErr
}

func TestWindowsServiceStepsInstallInDependencyOrderAndRollbackOnlyNewDeclarations(t *testing.T) {
	definitions := []windowsRuntimeServiceDefinition{
		{kind: service.HostdKind, executable: `C:\Paperboat\pb.exe`},
		{kind: service.DaemonKind, executable: `C:\Paperboat\pb.exe`},
		{kind: service.UpdaterKind, executable: `C:\Paperboat\pb.exe`},
	}
	present := map[string]bool{"daemon": true}
	var events []string
	updaterFailure := errors.New("updated service failed")
	makeStep := func(definition windowsRuntimeServiceDefinition) (windowsServicePlanStep, error) {
		return &fakeWindowsServicePlanStep{path: definition.kind, events: &events, installErr: map[string]error{service.UpdaterKind: updaterFailure}[definition.kind]}, nil
	}
	declarationPresent := func(_ context.Context, path string) (bool, error) { return present[path], nil }
	_, err := executeWindowsServiceSteps(context.Background(), definitions, makeStep, declarationPresent, false)
	if !errors.Is(err, updaterFailure) {
		t.Fatalf("install err=%v want updater failure", err)
	}
	want := []string{"install:hostd", "install:daemon", "install:updater", "uninstall:updater", "uninstall:hostd"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%q want=%q", events, want)
	}
}

func TestWindowsServiceStepsTreatStaleDeclarationAsNewWhenSCMIsAbsent(t *testing.T) {
	definitions := []windowsRuntimeServiceDefinition{
		{kind: service.HostdKind},
		{kind: service.DaemonKind},
	}
	// The hostd declaration is left behind by an interrupted install, but SCM
	// has no matching registration. The native registration snapshot, not the
	// declaration file, must determine rollback ownership.
	declarations := map[string]bool{service.HostdKind: true}
	registered := map[string]bool{}
	inspected := map[string]bool{}
	var events []string
	installFailure := errors.New("daemon service failed")
	makeStep := func(definition windowsRuntimeServiceDefinition) (windowsServicePlanStep, error) {
		return &fakeWindowsServicePlanStep{
			path:       definition.kind,
			events:     &events,
			installErr: map[string]error{service.DaemonKind: installFailure}[definition.kind],
		}, nil
	}
	preexisting := func(_ context.Context, path string) (bool, error) {
		inspected[path] = true
		return registered[path], nil
	}
	_, err := executeWindowsServiceSteps(context.Background(), definitions, makeStep, preexisting, false)
	if !errors.Is(err, installFailure) {
		t.Fatalf("install err=%v want daemon failure", err)
	}
	want := []string{"install:hostd", "install:daemon", "uninstall:daemon", "uninstall:hostd"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%q want=%q", events, want)
	}
	if !declarations[service.HostdKind] || registered[service.HostdKind] || !inspected[service.HostdKind] {
		t.Fatalf("stale declaration fixture was not inspected: declarations=%v registered=%v inspected=%v", declarations, registered, inspected)
	}
}

func TestWindowsServiceStepsCleanupAfterPostInstallReadinessFailure(t *testing.T) {
	definitions := []windowsRuntimeServiceDefinition{
		{kind: service.HostdKind},
		{kind: service.DaemonKind},
	}
	var events []string
	makeStep := func(definition windowsRuntimeServiceDefinition) (windowsServicePlanStep, error) {
		return &fakeWindowsServicePlanStep{path: definition.kind, events: &events}, nil
	}
	// The daemon was registered before this attempt. A post-install readiness
	// failure must remove only the hostd registration created by this attempt.
	preexisting := func(_ context.Context, path string) (bool, error) {
		return path == service.DaemonKind, nil
	}
	cleanup, err := executeWindowsServiceSteps(context.Background(), definitions, makeStep, preexisting, false)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the caller's local-daemon readiness failure by invoking the
	// returned cleanup before handing the failure back to its caller.
	if err := cleanup(); err != nil {
		t.Fatalf("post-install cleanup err=%v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("idempotent cleanup err=%v", err)
	}
	want := []string{"install:hostd", "install:daemon", "uninstall:hostd"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%q want=%q", events, want)
	}
}

func TestWindowsServiceStepsReadinessHookRunsBeforeUpdater(t *testing.T) {
	definitions := []windowsRuntimeServiceDefinition{
		{kind: service.HostdKind},
		{kind: service.DaemonKind},
		{kind: service.UpdaterKind},
	}
	var events []string
	makeStep := func(definition windowsRuntimeServiceDefinition) (windowsServicePlanStep, error) {
		return &fakeWindowsServicePlanStep{path: definition.kind, events: &events}, nil
	}
	_, err := executeWindowsServiceStepsWithHook(
		context.Background(), definitions, makeStep,
		func(context.Context, string) (bool, error) { return false, nil }, true,
		func(_ int, definition windowsRuntimeServiceDefinition) error {
			if definition.kind == service.DaemonKind {
				events = append(events, "ready:"+definition.kind)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"install:hostd", "start:hostd",
		"install:daemon", "start:daemon", "ready:daemon",
		"install:updater", "start:updater",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%q want=%q", events, want)
	}
}

func TestWindowsServiceStepsStartFailureRollsBackInReverseOrderWithLiveContext(t *testing.T) {
	definitions := []windowsRuntimeServiceDefinition{
		{kind: service.HostdKind},
		{kind: service.DaemonKind},
		{kind: service.UpdaterKind},
	}
	var events []string
	startFailure := errors.New("local daemon was not ready")
	makeStep := func(definition windowsRuntimeServiceDefinition) (windowsServicePlanStep, error) {
		return &fakeWindowsServicePlanStep{path: definition.kind, events: &events, startErr: map[string]error{service.DaemonKind: startFailure}[definition.kind]}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cleanup, err := executeWindowsServiceSteps(ctx, definitions, makeStep, func(context.Context, string) (bool, error) { return false, nil }, true)
	if !errors.Is(err, startFailure) {
		t.Fatalf("start err=%v want daemon failure", err)
	}
	want := []string{"install:hostd", "start:hostd", "install:daemon", "start:daemon", "uninstall:daemon", "uninstall:hostd"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%q want=%q", events, want)
	}
	if cleanup != nil {
		t.Fatal("failed service plan returned cleanup handle")
	}
	cancel()
}

func TestWindowsRuntimeServicesUseCanonicalDaemon(t *testing.T) {
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		t.Fatal(err)
	}
	definitions := windowsRuntimeServiceDefinitions(layout, "0123456789abcdef0123456789abcdef")
	if len(definitions) != 3 {
		t.Fatalf("definitions=%+v", definitions)
	}
	if definitions[0].kind != service.HostdKind || definitions[1].kind != service.DaemonKind || definitions[2].kind != service.UpdaterKind {
		t.Fatalf("service order=%+v", definitions)
	}
	for _, definition := range definitions {
		if definition.executable != layout.Binary {
			t.Fatalf("service %q executable=%q want=%q", definition.kind, definition.executable, layout.Binary)
		}
	}
	if !reflect.DeepEqual(definitions[0].arguments, []string{"daemon", "__runtime-hostd", "--generation", "0123456789abcdef0123456789abcdef"}) || !reflect.DeepEqual(definitions[1].arguments, []string{"daemon", "__runtime-local-daemon"}) || !reflect.DeepEqual(definitions[2].arguments, []string{"daemon", "__runtime-updated"}) {
		t.Fatalf("definitions=%+v", definitions)
	}
}

func TestWindowsHostServicesInstallSSHBeforeStartingRuntime(t *testing.T) {
	var events []string
	err := executeWindowsServiceInstallPlan(func() error {
		events = append(events, "ssh")
		return nil
	}, func() error {
		events = append(events, "recover")
		return nil
	}, func() error {
		events = append(events, "runtime")
		return nil
	}, func() error {
		events = append(events, "cleanup-ssh")
		return nil
	})
	if err != nil || !reflect.DeepEqual(events, []string{"ssh", "recover", "runtime"}) {
		t.Fatalf("events=%q err=%v", events, err)
	}
}

// A Windows guest without App Installer aborted pairing at the OpenSSH step (2026-11-10). Like Linux and macOS
// without sshd, the rest installs; only a machine that cannot supply OpenSSH is let through.
func TestWindowsPlansContinueWithoutObtainableOpenSSHAndAbortOnTrustFailures(t *testing.T) {
	cause := errors.New("exit status 1")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, test := range map[string]struct {
		ctx      context.Context
		err      error
		proceeds bool
	}{
		"inventory unavailable":   {context.Background(), errors.Join(windowsopenssh.ErrInstallerUnavailable, cause), true},
		"package install failed":  {context.Background(), errors.Join(windowsopenssh.ErrInstallFailed, cause), true},
		"untrusted binary":        {context.Background(), errors.Join(windowsopenssh.ErrUntrustedBinary, cause), false},
		"foreign service":         {context.Background(), windowsopenssh.ErrServiceOwnership, false},
		"unavailable and foreign": {context.Background(), errors.Join(windowsopenssh.ErrInstallerUnavailable, windowsopenssh.ErrServiceOwnership), false},
		"unclassified":            {context.Background(), cause, false},
		"cancelled installation":  {cancelled, errors.Join(windowsopenssh.ErrInstallerUnavailable, cause), false},
	} {
		for plan, run := range map[string]func(ssh, phase func() error, cleanup func() error) error{
			"install": func(ssh, phase, cleanup func() error) error {
				return executeWindowsServiceInstallPlan(ssh, phase, phase, cleanup)
			},
			"repair": func(ssh, phase, cleanup func() error) error {
				return executeWindowsServiceRepairPlan(ssh, phase, phase, phase, cleanup)
			},
		} {
			phases, cleaned := 0, false
			err := run(
				func() error { return skipUnobtainableWindowsSSH(test.ctx, test.err) },
				func() error { phases++; return nil },
				func() error { cleaned = true; return nil },
			)
			if test.proceeds {
				if err != nil || phases == 0 || cleaned {
					t.Errorf("%s %s: err=%v phases=%d cleaned=%t, want the runtime installed without OpenSSH", name, plan, err, phases, cleaned)
				}
			} else if !errors.Is(err, test.err) || phases != 0 {
				t.Errorf("%s %s: err=%v phases=%d, want the installation aborted with the cause", name, plan, err, phases)
			}
		}
	}
}

func TestWindowsHostServicesCleanSSHWhenRuntimeFails(t *testing.T) {
	var events []string
	runtimeFailure := errors.New("runtime failed")
	err := executeWindowsServiceInstallPlan(func() error {
		events = append(events, "ssh")
		return nil
	}, func() error {
		events = append(events, "recover")
		return nil
	}, func() error {
		events = append(events, "runtime")
		return runtimeFailure
	}, func() error {
		events = append(events, "cleanup-ssh")
		return nil
	})
	if !errors.Is(err, runtimeFailure) || !reflect.DeepEqual(events, []string{"ssh", "recover", "runtime", "cleanup-ssh"}) {
		t.Fatalf("events=%q err=%v", events, err)
	}
}

func TestWindowsHostServicesCleanSSHWhenRuntimeRecoveryFails(t *testing.T) {
	var events []string
	recoveryFailure := errors.New("recovery failed")
	err := executeWindowsServiceInstallPlan(func() error {
		events = append(events, "ssh")
		return nil
	}, func() error {
		events = append(events, "recover")
		return recoveryFailure
	}, func() error {
		events = append(events, "runtime")
		return nil
	}, func() error {
		events = append(events, "cleanup-ssh")
		return nil
	})
	if !errors.Is(err, recoveryFailure) || !reflect.DeepEqual(events, []string{"ssh", "recover", "cleanup-ssh"}) {
		t.Fatalf("events=%q err=%v", events, err)
	}
}

func TestWindowsRepairPlanKeepsSSHUntilFinalLifecycleRepair(t *testing.T) {
	var events []string
	err := executeWindowsServiceRepairPlan(func() error {
		events = append(events, "ssh")
		return nil
	}, func() error {
		events = append(events, "recover")
		return nil
	}, func() error {
		events = append(events, "binary-config")
		return nil
	}, func() error {
		events = append(events, "lifecycle-repair")
		return nil
	}, func() error {
		events = append(events, "cleanup-ssh")
		return nil
	})
	if err != nil || !reflect.DeepEqual(events, []string{"ssh", "recover", "binary-config", "lifecycle-repair"}) {
		t.Fatalf("events=%q err=%v", events, err)
	}
}

func TestWindowsRepairPlanCleansSSHOnlyAfterARepairPhaseFails(t *testing.T) {
	for _, test := range []struct {
		name       string
		failureAt  string
		wantEvents []string
	}{
		{name: "recovery", failureAt: "recover", wantEvents: []string{"ssh", "recover", "cleanup-ssh"}},
		{name: "binary-config", failureAt: "binary-config", wantEvents: []string{"ssh", "recover", "binary-config", "cleanup-ssh"}},
		{name: "lifecycle", failureAt: "lifecycle-repair", wantEvents: []string{"ssh", "recover", "binary-config", "lifecycle-repair", "cleanup-ssh"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			failure := errors.New(test.failureAt + " failed")
			phase := func(name string) func() error {
				return func() error {
					events = append(events, name)
					if name == test.failureAt {
						return failure
					}
					return nil
				}
			}
			err := executeWindowsServiceRepairPlan(phase("ssh"), phase("recover"), phase("binary-config"), phase("lifecycle-repair"), phase("cleanup-ssh"))
			if !errors.Is(err, failure) || !reflect.DeepEqual(events, test.wantEvents) {
				t.Fatalf("events=%q err=%v", events, err)
			}
		})
	}
}

func TestWindowsActivatorOwnershipAcceptsOnlyVersionedReleaseBinary(t *testing.T) {
	layout, err := service.WindowsUserLayout("S-1-5-21-196874002-3579808280-620482841-1030")
	if err != nil {
		t.Fatal(err)
	}
	valid := layout.ReleasesRoot + `\versions\2026.08.28.1\pb.exe`
	if !windowsActivatorServiceOwned(layout, valid, []string{"daemon", "__runtime-activate", "--instance", layout.Instance}, "LocalSystem") {
		t.Fatalf("owned activator target rejected: %q", valid)
	}
	for _, invalid := range []string{layout.Binary, layout.BinaryRollback, layout.ReleasesRoot + `\versions\..\pb.exe`, layout.ReleasesRoot + `\versions\2026.08.28.1\other.exe`} {
		if windowsActivatorExecutableOwned(layout, invalid) {
			t.Fatalf("unowned activator target accepted: %q", invalid)
		}
	}
	if windowsActivatorServiceOwned(layout, valid, []string{"daemon", "__runtime-updated", "--instance", layout.Instance}, "LocalSystem") || windowsActivatorServiceOwned(layout, valid, []string{"daemon", "__runtime-activate", "--instance", layout.Instance}, "User") || windowsActivatorServiceOwned(layout, valid, []string{"daemon", "__runtime-activate", "--instance", "u000000000000000000000000"}, "LocalSystem") {
		t.Fatal("unowned activator service command accepted")
	}
	if windowsActivatorServiceOwned(layout, valid, []string{"daemon", "__runtime-activate"}, "LocalSystem") || windowsActivatorServiceOwned(layout, valid, []string{"daemon", "__runtime-activate", "--instance", ""}, "LocalSystem") {
		t.Fatal("missing instance accepted")
	}
	empty := layout
	empty.Instance = ""
	if windowsActivatorServiceOwned(empty, valid, []string{"daemon", "__runtime-activate", "--instance", ""}, "LocalSystem") {
		t.Fatal("unscoped activator accepted")
	}
}

func TestWindowsServiceStartFailureIdentifiesRoleAndRetainsCause(t *testing.T) {
	var events []string
	cause := context.DeadlineExceeded
	_, err := executeWindowsServiceSteps(context.Background(), []windowsRuntimeServiceDefinition{{kind: service.HostdKind}}, func(definition windowsRuntimeServiceDefinition) (windowsServicePlanStep, error) {
		return &fakeWindowsServicePlanStep{path: definition.kind, events: &events, startErr: cause}, nil
	}, func(context.Context, string) (bool, error) { return true, nil }, true)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "start Windows hostd service") {
		t.Fatalf("lost service failure context: %v", err)
	}
}

func TestWindowsServiceCleanupRetainsFirstFailureWithoutRepeatingMutation(t *testing.T) {
	var events []string
	failure := errors.New("PRIVATE cleanup cause")
	cleanup, err := executeWindowsServiceSteps(context.Background(), []windowsRuntimeServiceDefinition{{kind: service.HostdKind}}, func(def windowsRuntimeServiceDefinition) (windowsServicePlanStep, error) {
		return &fakeWindowsServicePlanStep{path: def.kind, events: &events, uninstallErr: failure}, nil
	}, func(context.Context, string) (bool, error) { return false, nil }, false)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		err := cleanup()
		if !errors.Is(err, failure) || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("cleanup cause or privacy lost: %T", err)
		}
	}
	if !reflect.DeepEqual(events, []string{"install:hostd", "uninstall:hostd"}) {
		t.Fatal("cleanup repeated owned mutation")
	}
}

// winpool-3: a machine where OpenSSH is only reachable through the PowerShell Gallery spent the whole 3 min of the
// bridge in that step and the pair rolled back. The step has its own budget; its expiry is announced and skips managed
// SSH, but a trust failure never is, whether it comes before or after the budget.
func TestWindowsSSHPreparationOutOfTimeIsAnnouncedAndTrustFailuresStillAbort(t *testing.T) {
	previous, previousNotice := windowsSSHPreparationBudget, windowsSSHTimeoutNotice
	t.Cleanup(func() { windowsSSHPreparationBudget, windowsSSHTimeoutNotice = previous, previousNotice })
	windowsSSHPreparationBudget = 30 * time.Millisecond
	var announced []error
	windowsSSHTimeoutNotice = func(err error) { announced = append(announced, err) }

	started := time.Now()
	err := prepareWindowsSSHWithin(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil || len(announced) != 1 || !errors.Is(announced[0], context.DeadlineExceeded) || time.Since(started) > 5*time.Second {
		t.Fatalf("a step out of its budget: err=%v announced=%v after %v", err, announced, time.Since(started))
	}

	// the package manager was killed at the budget: its failure is unobtainable, not a trust failure
	killed := fmt.Errorf("%w: signal: killed", windowsopenssh.ErrInstallFailed)
	if err := prepareWindowsSSHWithin(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		return killed
	}); err != nil || len(announced) != 2 {
		t.Fatalf("a killed package manager: err=%v announced=%d", err, len(announced))
	}

	refusal := windowsopenssh.ErrUntrustedBinary
	if err := prepareWindowsSSHWithin(context.Background(), func(context.Context) error { return refusal }); !errors.Is(err, refusal) {
		t.Fatalf("a trust failure was skipped: %v", err)
	}
	// the race the review found: the refusal arrives after the budget expired
	if err := prepareWindowsSSHWithin(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		return refusal
	}); !errors.Is(err, refusal) || len(announced) != 2 {
		t.Fatalf("a trust failure after the budget was skipped: err=%v announced=%d", err, len(announced))
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if err := prepareWindowsSSHWithin(parent, func(ctx context.Context) error { return ctx.Err() }); err == nil {
		t.Fatal("the caller's own cancellation was skipped")
	}
}

// A skipped managed SSH preparation is recorded as an observed fault: the elevated helper's stderr notice reaches
// no one, so the skip must not look like a clean install.
func TestSkippedWindowsSSHIsAnObservedFault(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	t.Cleanup(restore)
	if err := skipUnobtainableWindowsSSH(context.Background(), fmt.Errorf("%w: no App Installer", windowsopenssh.ErrInstallFailed)); err != nil {
		t.Fatalf("an unobtainable package aborted: %v", err)
	}
	if len(faults) != 1 || faults[0].Code != "managed_ssh_failed" {
		t.Fatalf("faults = %+v, want one managed_ssh_failed", faults)
	}
}
