//go:build windows

package hostruntimecmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	gort "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/processlaunch"
	"golang.org/x/sys/windows"
)

const windowsOwnerWorkloadEnvironment = "PAPERBOAT_WINDOWS_OWNER_WORKLOAD"

// runHostd owns Windows workloads for the service lifetime and starts the
// replaceable worker only through the authenticated named-pipe fence.
func runHostd(ctx context.Context, args []string, output io.Writer) (err error) {
	instance, err := resolveWindowsRuntimeInstance(args)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if sid, sidErr := currentWindowsSID(); sidErr == nil && sid == instance.config.OwnerSID {
				recordWindowsOwnerLaunchFailure(instance.config.StateRoot, err)
			} else {
				recordWindowsServiceLaunchFailure("PaperboatHostd-worker-"+instance.name, err)
			}
		}
	}()
	return runHostdInner(ctx, output, instance)
}

func recordWindowsOwnerLaunchFailure(stateRoot string, err error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return
	}
	recordWindowsLaunchFailure(filepath.Join(stateRoot, "hostd-startup-error.log"), err)
}

func runHostdInner(ctx context.Context, output io.Writer, instance windowsRuntimeInstance) error {
	install := instance.config
	// The SCM parent launches the owner workload with CreateProcessAsUser.
	// That child is deliberately not an SCM service process. Relying solely on
	// an inherited environment marker is fragile on Windows because token
	// environment construction can omit overrides under S4U. Use the process
	// context as the authoritative boundary, while retaining the marker for
	// explicit diagnostics and older service environments.
	if os.Getenv(windowsOwnerWorkloadEnvironment) != "1" {
		// CreateProcessAsUser intentionally launches the owner workload without
		// the SCM dispatcher. On Windows, svc.IsWindowsService can still report
		// true for that detached process because it has no console, so the SID is
		// the authoritative boundary. LocalSystem, which owns the SCM parent,
		// cannot match the enrolled owner SID and continues into the dispatcher.
		if sid, sidErr := currentWindowsSID(); sidErr == nil && sid == install.OwnerSID {
			return runOwnerHostd(ctx, output, install)
		}
		return runWindowsHostdService(install, instance)
	}
	if sid, sidErr := currentWindowsSID(); sidErr != nil || sid != install.OwnerSID {
		return errors.New("Paperboat Windows hostd workload is not running as the enrolled owner")
	}
	return runOwnerHostd(ctx, output, install)
}

func windowsHostdWorkerEnvironment(install hostinstall.WindowsRuntimeConfig, layout service.Layout, runtimeExecutable string) (map[string]string, error) {
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	shell := filepath.Join(systemDirectory, "cmd.exe")
	if err := validateWindowsWorkerExecutable(shell); err != nil {
		return nil, errors.Join(errors.New("Paperboat Windows command shell is unavailable"), err)
	}
	instanceRoot, err := hostinstall.WindowsInstanceRoot(install.Instance)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		windowsOwnerWorkloadEnvironment:    "1",
		"PAPERBOAT_WINDOWS_OWNER_SID":      install.OwnerSID,
		"PAPERBOAT_SESSION_OWNER_CATALOG":  filepath.Join(instanceRoot, "session-owners"),
		"PAPERBOAT_UPDATED_SOCKET":         layout.UpdaterSocket,
		"PAPERBOAT_HOSTD_SOCKET":           layout.HostdSocket,
		"PAPERBOAT_HOSTD_TOKEN_FILE":       install.TokenFile,
		"PAPERBOAT_RUNTIME_CURRENT":        runtimeExecutable,
		"PAPERBOAT_BINARY_ROLLBACK":        layout.BinaryRollback,
		"PAPERBOAT_BINARY_STAGED":          layout.BinaryStaged,
		"PAPERBOAT_RUNTIME_STATE_ROOT":     install.StateRoot,
		"PAPERBOAT_WORKSPACE_ROOT":         install.Workspace,
		"PAPERBOAT_CONTROL_URL":            install.ControlURL,
		"PAPERBOAT_RUNTIME_LISTEN_ADDRESS": install.ListenAddress,
		"PAPERBOAT_MACHINE_ID":             install.MachineID,
		"PAPERBOAT_RUNTIME_SERVICE_SCOPE":  "user",
		// CreateEnvironmentBlock does not guarantee ComSpec for S4U and
		// service-created owner tokens. Pin the native system shell explicitly;
		// the production host validates this value before starting any session.
		"PAPERBOAT_SHELL": shell,
	}, nil
}

// runWindowsHostdService is the only SCM-facing hostd path. The child marker
// is generated here, not accepted from the installed service definition, so a
// LocalSystem process cannot accidentally run the workload itself.
func runWindowsHostdService(install hostinstall.WindowsRuntimeConfig, instance windowsRuntimeInstance) error {
	layout := instance.layout
	hostdExecutable, executableErr := os.Executable()
	if executableErr != nil {
		return executableErr
	}
	if _, err := service.VerifyOwnedWindowsRoleExecutable(service.HostdKind, layout.Instance, hostdExecutable); err != nil {
		return err
	}
	runtimeExecutable := layout.Binary
	identity, err := updated.WindowsRuntimeSource(context.Background(), windowsUpdatedConfigFor(install, layout, install.Source.Version))
	if err != nil {
		return err
	}
	carrierPath, err := hostinstall.CarrierConfigPath(layout.Instance)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(carrierPath); err == nil {
		if _, err := hostinstall.ReadCarrierConfig(context.Background(), layout.Instance); err != nil {
			return err
		}
		runtimeExecutable, err = hostinstall.WindowsPinnedSourcePath(layout, identity)
		if err != nil {
			return err
		}
		if err := identity.Verify(runtimeExecutable); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	environment, err := windowsHostdWorkerEnvironment(install, layout, runtimeExecutable)
	if err != nil {
		return err
	}
	version := identity.Version
	environment["PAPERBOAT_FEATURE_VERSION"] = version
	environment["PAPERBOAT_FEATURE_SHA256"] = identity.SHA256
	environment["PAPERBOAT_FEATURE_LENGTH"] = strconv.FormatInt(identity.Length, 10)

	return service.RunWindowsService(service.ServiceEntryConfig{
		Name:        windowsInstanceServiceName("PaperboatHostd", instance.name),
		Executable:  hostdExecutable,
		Arguments:   []string{"daemon", "__runtime-hostd", "--instance", instance.name},
		EnrolledSID: install.OwnerSID,
		Environment: environment,
		LaunchFailure: func(err error) {
			recordWindowsServiceLaunchFailure(windowsInstanceServiceName("PaperboatHostd", instance.name), err)
		},
		StartPrivilegedSidecar: func(ctx context.Context) (service.PrivilegedSidecar, error) {
			sidecarCtx, cancelSidecars := context.WithCancel(ctx)
			results := make(chan error, 1)
			availabilityReady := make(chan struct{})
			updateDiagnostics, err := hostservice.NewWindowsUpdateDiagnostics(layout.UpdateStateRoot, install.MachineID)
			if err != nil {
				cancelSidecars()
				return service.PrivilegedSidecar{}, err
			}
			authorizedKeys, err := hostservice.NewWindowsAuthorizedKeys(instance.name)
			if err != nil {
				cancelSidecars()
				return service.PrivilegedSidecar{}, err
			}
			hostServiceSocket, err := hostservice.WindowsSocketPath(instance.name)
			if err != nil {
				cancelSidecars()
				return service.PrivilegedSidecar{}, err
			}
			instanceRoot, err := hostinstall.WindowsInstanceRoot(instance.name)
			if err != nil {
				cancelSidecars()
				return service.PrivilegedSidecar{}, err
			}
			availability, err := hostservice.New(hostservice.Config{
				SocketPath:        hostServiceSocket,
				StatePath:         filepath.Join(instanceRoot, "availability-policy.json"),
				SID:               install.OwnerSID,
				Applier:           hostservice.NewPlatformApplier(filepath.Join(instanceRoot, "power-baseline.json")),
				Version:           buildinfo.Version,
				UpdateDiagnostics: updateDiagnostics,
				AuthorizedKeys:    authorizedKeys,
				Ready: func() error {
					close(availabilityReady)
					return nil
				},
			})
			if err != nil {
				cancelSidecars()
				return service.PrivilegedSidecar{}, err
			}
			go func() { results <- availability.Run(sidecarCtx) }()
			for availabilityReady != nil {
				select {
				case sidecarErr := <-results:
					cancelSidecars()
					return service.PrivilegedSidecar{}, sidecarErr
				case <-availabilityReady:
					availabilityReady = nil
				case <-time.After(15 * time.Second):
					cancelSidecars()
					return service.PrivilegedSidecar{}, errors.New("Windows privileged host sidecars did not become ready")
				}
			}
			done := make(chan error, 1)
			go func() {
				first := <-results
				cancelSidecars()
				done <- first
			}()
			return service.PrivilegedSidecar{Done: done}, nil
		},
	})
}

func recordWindowsServiceLaunchFailure(name string, err error) {
	recordWindowsLaunchFailure(filepath.Join(hostinstall.WindowsProgramDataRoot(), name+"-startup-error.log"), err)
}

func recordWindowsLaunchFailure(path string, cause error) {
	fault := errorreport.Current().ObserveFailure(context.Background(), "paperboat-daemon", "daemon", "command", "command_failed", cause)
	body, err := json.Marshal(fault)
	if err == nil {
		err = os.WriteFile(path, append(body, '\n'), 0600)
	}
	if err != nil {
		observeRuntimeFailure(context.Background(), "diagnostic", runtimeFailure("runtime startup diagnostics could not be saved", err))
	}
}

func runOwnerHostd(ctx context.Context, output io.Writer, install hostinstall.WindowsRuntimeConfig) (resultErr error) {
	socket, tokenPath, executable := os.Getenv("PAPERBOAT_HOSTD_SOCKET"), os.Getenv("PAPERBOAT_HOSTD_TOKEN_FILE"), os.Getenv("PAPERBOAT_RUNTIME_CURRENT")
	if socket == "" {
		socket = `\\.\pipe\PaperboatHostd`
	}
	if tokenPath == "" {
		tokenPath = install.TokenFile
	}
	if executable == "" {
		layout, layoutErr := hostinstall.WindowsLayoutForInstance(install.Instance)
		if layoutErr != nil {
			return layoutErr
		}
		executable = layout.Binary
	}
	if !validWindowsPipe(socket) {
		return fmt.Errorf("hostd worker named-pipe configuration is invalid: %q", socket)
	}
	if !filepath.IsAbs(tokenPath) || filepath.Clean(tokenPath) != tokenPath {
		return errors.New("hostd worker token-file configuration is invalid")
	}
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return errors.New("hostd worker executable configuration is invalid")
	}
	token, err := readWindowsHostdTokenForSID(tokenPath, install.OwnerSID)
	if err != nil {
		return err
	}
	if err := validateWindowsWorkerExecutable(executable); err != nil {
		return err
	}
	sid := install.OwnerSID
	if sid == "" {
		sid, err = currentWindowsSID()
		if err != nil {
			return err
		}
	}
	host, err := hostruntime.NewProductionOwner(ctx, buildinfo.Version, os.Getenv)
	if err != nil {
		return err
	}
	if err := host.StartHostd(ctx); err != nil {
		return err
	}

	var owned *ownedWorkers
	var stopServer context.CancelFunc
	var serverDone chan error
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		var workerErr, serverErr error
		if owned != nil {
			workerErr = owned.shutdown(shutdownCtx)
		}
		if stopServer != nil {
			stopServer()
			select {
			case serverErr = <-serverDone:
			case <-shutdownCtx.Done():
				serverErr = runtimeContextFailure(shutdownCtx)
			}
		}
		resultErr = withCleanup("hostd failed or its cleanup did not finish", resultErr, workerErr, serverErr, host.ShutdownHostd(shutdownCtx))
	}()
	statePath, err := windowsHostdFencePath()
	if err != nil {
		return err
	}
	if err := ensureWindowsHostdStateDirectory(filepath.Dir(statePath), sid); err != nil {
		return err
	}

	version := os.Getenv("PAPERBOAT_FEATURE_VERSION")
	length, lengthErr := strconv.ParseInt(os.Getenv("PAPERBOAT_FEATURE_LENGTH"), 10, 64)
	identity := installsource.Source{Version: version, Platform: "windows", Architecture: gort.GOARCH, SHA256: os.Getenv("PAPERBOAT_FEATURE_SHA256"), Length: length, Distribution: install.Source.Distribution}
	if lengthErr != nil || identity.Validate() != nil {
		return errors.New("native owner feature identity is invalid")
	}
	if err := identity.Verify(executable); err != nil {
		return err
	}

	template := workerStartRequest{Executable: executable, Version: version, APIMin: 1, APIMax: 1, WorkerID: "runtime-" + strings.ReplaceAll(version, " ", "-"), HostdEndpoint: socket, Capability: token, MutationsDisabled: true}
	owned = newOwnedWorkers(ctx, template, func(ctx context.Context, r workerStartRequest) (workerProcess, error) {
		return startWindowsRuntimeWorkerForRelease(ctx, r.Executable, socket, tokenPath, sid, r.WorkerID, r.Version, r.APIMin, r.APIMax)
	})

	server, err := hostdproto.NewServer(hostdproto.SocketConfig{SocketPath: socket, StatePath: statePath, SID: sid, Token: token, APIMin: 1, APIMax: 1, Workloads: host.WorkloadStatus, DrainExecutions: host.DrainExecutions, UpdateGate: host.UpdateGate(), WorkerControl: owned, RequestTimeout: 31 * time.Minute,
		MaintenancePending: !install.Committed,
		VerifyMaintenanceCommitted: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			committed, err := hostinstall.LoadWindowsRuntimeConfigForInstance(install.Instance)
			if err != nil {
				return err
			}
			if committed.OwnerSID != install.OwnerSID || committed.Instance != install.Instance || !committed.Committed || committed.Source != install.Source {
				return hostinstall.ErrInvalidRequest
			}
			return nil
		}})
	if err != nil {
		return err
	}
	host.BindLifecycle(socket+".workloads", token, server.Status, server.AcquireActive)
	if err := host.StartBridge(ctx); err != nil {
		return err
	}
	serverCtx, serverCancel := context.WithCancel(ctx)
	stopServer = serverCancel
	serverDone = make(chan error, 1)
	go func() { defer close(serverDone); serverDone <- server.Run(serverCtx) }()
	if err := waitForWindowsHostdPipe(ctx, socket, token, serverDone); err != nil {
		stopServer()
		return err
	}
	_, err = owned.HandleWorkerControl(ctx, hostdproto.WorkerControlRequest{Operation: "start", WorkerID: template.WorkerID, Executable: executable, Version: template.Version, APIMin: 1, APIMax: 1})
	if err == nil {
		_, err = owned.HandleWorkerControl(ctx, hostdproto.WorkerControlRequest{Operation: "ready", WorkerID: template.WorkerID})
	}
	if err == nil {
		_, err = owned.HandleWorkerControl(ctx, hostdproto.WorkerControlRequest{Operation: "activate", WorkerID: template.WorkerID})
	}
	if err != nil {
		stopServer()
		return err
	}
	if _, err := fmt.Fprintln(output, "pb daemon hostd ready"); err != nil {
		return runtimeFailure("hostd readiness could not be written", err)
	}
	select {
	case <-ctx.Done():
		return runtimeShutdownCause(ctx)
	case serverErr := <-serverDone:
		if ctx.Err() != nil && serverErr == nil {
			return runtimeShutdownCause(ctx)
		}
		return runtimeFailure("hostd lifecycle listener stopped", serverErr)
	}
}

func ensureWindowsHostdStateDirectory(path, sidValue string) error {
	sid, err := windows.StringToSid(sidValue)
	if err != nil || sid == nil || !sid.IsValid() || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("hostd state directory configuration is invalid")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return runtimeFailure("hostd state directory is unsafe", err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sidValue + ")")
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if err := setWindowsHostdStateSecurity(path, sid, dacl); err != nil {
		return err
	}
	// Older SYSTEM-owned hostd releases created the persisted fence with an
	// Administrators owner. Repair that exact owned state during upgrade before
	// the owner-scoped worker validates it.
	fencePath := filepath.Join(path, "fence.json")
	if info, statErr := os.Lstat(fencePath); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("hostd fence state is unsafe")
		}
		if err := setWindowsHostdStateSecurity(fencePath, sid, dacl); err != nil {
			return err
		}
	} else if !runtimeCausesOnly(statErr, os.ErrNotExist) {
		return statErr
	}
	return nil
}

func setWindowsHostdStateSecurity(path string, owner *windows.SID, dacl *windows.ACL) error {
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
}

func runWorker(ctx context.Context, args []string, input io.Reader, output, stderr io.Writer) error {
	return runWorkerWith(ctx, args, input, output, stderr, newWorkerFeature)
}
func runWorkerWith(ctx context.Context, args []string, input io.Reader, output, stderr io.Writer, newFeature workerFeatureFactory) (resultErr error) {
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "hostd lifecycle named pipe")
	tokenPath := flags.String("token-file", "", "hostd capability token")
	ownerSID := flags.String("owner-sid", "", "enrolled Windows owner SID")
	workerID := flags.String("worker-id", "", "runtime worker identity")
	version := flags.String("version", buildinfo.Version, "runtime version")
	apiMin := flags.Uint("api-min", 1, "minimum hostd API")
	apiMax := flags.Uint("api-max", 1, "maximum hostd API")
	heartbeat := flags.Duration("heartbeat", 5*time.Second, "hostd heartbeat interval")
	waitActivation := flags.Bool("wait-activation", false, "wait for private supervisor activation")
	if flags.Parse(args) != nil {
		return errors.New("invalid worker invocation")
	}
	parsedOwnerSID, ownerSIDErr := windows.StringToSid(*ownerSID)
	if flags.NArg() != 0 || !validWindowsPipe(*socket) || !filepath.IsAbs(*tokenPath) || filepath.Clean(*tokenPath) != *tokenPath || ownerSIDErr != nil || parsedOwnerSID == nil || !parsedOwnerSID.IsValid() || parsedOwnerSID.String() != *ownerSID || *workerID == "" || *version == "" || *apiMin == 0 || *apiMin > 1024 || *apiMax < *apiMin || *apiMax > 1024 || *heartbeat < time.Second || *heartbeat > time.Minute {
		return errors.New("invalid worker invocation")
	}
	token, err := readWindowsHostdTokenForSID(*tokenPath, *ownerSID)
	if err != nil {
		return err
	}
	client, err := hostdproto.NewClient(*socket, token, 5*time.Second)
	if err != nil {
		return err
	}
	candidate, err := hostdproto.NewCandidate(client, *workerID, *version, uint16(*apiMin), uint16(*apiMax))
	if err != nil {
		return err
	}
	ready, err := candidate.Ready(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "ready %d %d\n", ready.Epoch, ready.APIVersion)
	if *waitActivation {
		if err := awaitWindowsWorkerActivation(ctx, input); err != nil {
			return err
		}
	}
	active, err := candidate.Activate(ctx)
	if err != nil {
		return err
	}
	feature, err := newFeature(ctx, *socket, token, *workerID, active.Epoch, *version)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := feature.Shutdown(shutdownCtx); cleanupErr != nil {
			resultErr = withCleanup("runtime worker failed or its cleanup did not finish", resultErr, cleanupErr)
		}
	}()
	// Activation fences the lease before feature startup. Readiness means the
	// feature is usable and its heartbeat is fresh after that startup completes.
	if err := feature.Health(ctx); err != nil {
		return err
	}
	if err := candidate.Heartbeat(ctx); err != nil {
		return runtimeFailure("hostd worker heartbeat failed", err)
	}
	if _, err := fmt.Fprintf(output, "active %d %d\n", active.Epoch, active.APIVersion); err != nil {
		return runtimeFailure("hostd worker activation output could not be written", err)
	}
	ticker := time.NewTicker(*heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return runtimeShutdownCause(ctx)
		case <-ticker.C:
			if err := feature.Health(ctx); err != nil {
				return err
			}
			if err := candidate.Heartbeat(ctx); err != nil {
				return runtimeFailure("hostd worker heartbeat failed", err)
			}
		}
	}
}

func awaitWindowsWorkerActivation(ctx context.Context, input io.Reader) error {
	return awaitWorkerActivation(ctx, input)
}

func currentWindowsSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return "", runtimeFailure("Windows hostd enrolled SID is unavailable", err)
	}
	return user.User.Sid.String(), nil
}

func windowsHostdFencePath() (string, error) {
	if value := os.Getenv("PAPERBOAT_HOSTD_FENCE_FILE"); value != "" {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return "", errors.New("Paperboat Windows hostd fence path is invalid")
		}
		return value, nil
	}
	root := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", errors.New("Paperboat Windows runtime state path is invalid")
	}
	return filepath.Join(root, "hostd", "fence.json"), nil
}

func validWindowsPipe(value string) bool {
	return strings.HasPrefix(strings.ToLower(value), `\\.\pipe\`) && len(value) > len(`\\.\pipe\`) && !strings.ContainsAny(value, "\x00\r\n")
}

func validateWindowsWorkerExecutable(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.EqualFold(filepath.Ext(path), ".exe") {
		return errors.New("Paperboat Windows runtime executable is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return runtimeFailure("Paperboat Windows runtime executable is unavailable", err)
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return runtimeFailure("Paperboat Windows runtime executable is unsafe", err)
	}
	return nil
}

func waitForWindowsHostdPipe(ctx context.Context, socket string, token []byte, done <-chan error) error {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var lastErr error
	for {
		client, err := hostdproto.NewClient(socket, token, time.Second)
		if err == nil {
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			_, err = client.Active(probeCtx)
			cancel()
			if err == nil {
				return nil
			}
		}
		if err != nil && !runtimeCausesOnly(err, os.ErrNotExist, context.Canceled) {
			lastErr = err
		}
		select {
		case serverErr := <-done:
			return runtimeFailure("hostd lifecycle listener exited before readiness", serverErr, lastErr)
		case <-ctx.Done():
			return runtimeFailure("hostd lifecycle readiness canceled", runtimeContextFailure(ctx), lastErr)
		case <-deadline.C:
			return runtimeFailure("hostd lifecycle named pipe did not become ready", context.DeadlineExceeded, lastErr)
		case <-tick.C:
		}
	}
}

type windowsRuntimeWorker struct {
	command    *exec.Cmd
	control    io.WriteCloser
	lines      *bufio.Reader
	workerID   string
	mu         sync.Mutex
	ready      hostdproto.Status
	stdout     io.ReadCloser
	outputOnce sync.Once
	outputErr  error
	waitOnce   sync.Once
	waitDone   chan struct{}
	waitErr    error
}

func startWindowsRuntimeWorker(ctx context.Context, executable, socket, tokenPath, ownerSID, workerID string) (*windowsRuntimeWorker, error) {
	return startWindowsRuntimeWorkerForRelease(ctx, executable, socket, tokenPath, ownerSID, workerID, buildinfo.Version, 1, 1)
}

func startWindowsRuntimeWorkerForRelease(ctx context.Context, executable, socket, tokenPath, ownerSID, workerID, version string, apiMin, apiMax uint16) (*windowsRuntimeWorker, error) {
	sid, sidErr := windows.StringToSid(ownerSID)
	if sidErr != nil || sid == nil || !sid.IsValid() || sid.String() != ownerSID || version == "" || apiMin == 0 || apiMin > apiMax || apiMax > 1024 {
		return nil, runtimeFailure("Windows runtime worker release parameters are invalid", sidErr)
	}
	command := exec.CommandContext(ctx, executable, "daemon", "__runtime-worker", "--socket", socket, "--token-file", tokenPath, "--owner-sid", ownerSID, "--worker-id", workerID, "--version", version, "--api-min", strconv.FormatUint(uint64(apiMin), 10), "--api-max", strconv.FormatUint(uint64(apiMax), 10), "--wait-activation")
	processlaunch.ConfigureBackground(command)
	control, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, runtimeFailure("starting the runtime worker failed", err, control.Close())
	}
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return nil, runtimeFailure("starting the runtime worker failed", err, control.Close(), stdout.Close())
	}
	return &windowsRuntimeWorker{command: command, control: control, lines: bufio.NewReader(io.LimitReader(stdout, 128)), workerID: workerID, stdout: stdout}, nil
}

func (w *windowsRuntimeWorker) Ready(ctx context.Context) (hostdproto.Status, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ready.Epoch != 0 {
		return w.ready, nil
	}
	line, err := readWindowsWorkerLine(ctx, w.lines, w.closeOutput)
	if err != nil {
		return hostdproto.Status{}, err
	}
	status, err := parseWindowsWorkerStatus(line, "ready", w.workerID)
	if err == nil {
		w.ready = status
	}
	return status, err
}

func (w *windowsRuntimeWorker) Activate(ctx context.Context) (hostdproto.Status, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ready.Epoch == 0 || w.control == nil {
		return hostdproto.Status{}, errors.New("worker is not ready")
	}
	if _, err := io.WriteString(w.control, "activate\n"); err != nil {
		return hostdproto.Status{}, err
	}
	if err := w.control.Close(); err != nil {
		return hostdproto.Status{}, err
	}
	w.control = nil
	line, err := readWindowsWorkerLine(ctx, w.lines, w.closeOutput)
	if err != nil {
		return hostdproto.Status{}, err
	}
	status, err := parseWindowsWorkerStatus(line, "active", w.workerID)
	if err != nil || status.Epoch != w.ready.Epoch || status.APIVersion != w.ready.APIVersion {
		return hostdproto.Status{}, errors.New("invalid Windows worker activation")
	}
	return status, nil
}

func (w *windowsRuntimeWorker) closeOutput() error {
	w.outputOnce.Do(func() {
		if w.stdout != nil {
			w.outputErr = w.stdout.Close()
		}
	})
	return w.outputErr
}
func (w *windowsRuntimeWorker) Stop(ctx context.Context) error {
	if w == nil || w.command == nil || w.command.Process == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	closeErr := w.closeOutput()
	w.waitOnce.Do(func() {
		w.waitDone = make(chan struct{})
		go func() { w.waitErr = w.command.Wait(); close(w.waitDone) }()
	})
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var stopErr error
	select {
	case <-w.waitDone:
	case <-ctx.Done():
		stopErr = runtimeContextFailure(ctx)
	case <-timer.C:
	}
	select {
	case <-w.waitDone:
	default:
		killErr := w.command.Process.Kill()
		if killErr != nil && !runtimeCausesOnly(killErr, os.ErrProcessDone) {
			stopErr = runtimeFailure("stopping the runtime worker failed", stopErr, killErr)
		}
		<-w.waitDone
	}
	// An exit status after fencing or explicit termination proves reaping. Other
	// Wait failures are retained; a second caller uses this same completed Wait.
	var exit *exec.ExitError
	waitErr := w.waitErr
	if errors.As(waitErr, &exit) {
		waitErr = nil
	}
	return runtimeFailureIfAny("runtime worker cleanup failed", stopErr, closeErr, waitErr)
}

func readWindowsWorkerLine(ctx context.Context, reader *bufio.Reader, closeInput func() error) (string, error) {
	type result struct {
		line string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() { line, err := reader.ReadString('\n'); resultCh <- result{line, err} }()
	select {
	case <-ctx.Done():
		closeErr := closeInput()
		result := <-resultCh
		readErr := result.err
		if runtimeCausesOnly(readErr, os.ErrClosed, io.EOF) {
			readErr = nil
		}
		return "", runtimeFailure("worker lifecycle read canceled", runtimeContextFailure(ctx), closeErr, readErr)
	case result := <-resultCh:
		if result.err != nil || len(result.line) > 64 {
			if result.err != nil {
				return "", result.err
			}
			return "", errors.New("worker lifecycle response exceeds limit")
		}
		return result.line, nil
	}
}

func parseWindowsWorkerStatus(line, state, workerID string) (hostdproto.Status, error) {
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[0] != state || workerID == "" {
		return hostdproto.Status{}, errors.New("invalid Windows worker lifecycle response")
	}
	var epoch uint64
	var api uint16
	if _, err := fmt.Sscanf(parts[1], "%d", &epoch); err != nil || epoch == 0 {
		return hostdproto.Status{}, runtimeFailure("invalid Windows worker lifecycle response", err)
	}
	if _, err := fmt.Sscanf(parts[2], "%d", &api); err != nil || api == 0 {
		return hostdproto.Status{}, runtimeFailure("invalid Windows worker lifecycle response", err)
	}
	hostState := hostdproto.StateCandidate
	if state == "active" {
		hostState = hostdproto.StateActive
	}
	return hostdproto.Status{State: hostState, WorkerID: workerID, Epoch: epoch, APIVersion: api}, nil
}

var _ = gort.GOARCH
