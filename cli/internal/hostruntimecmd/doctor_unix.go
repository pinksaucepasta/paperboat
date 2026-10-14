//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtimeport"
)

type doctorCheck struct {
	Code     string `json:"code"`
	State    string `json:"state"`
	Message  string `json:"message"`
	Recovery string `json:"recovery,omitempty"`
}

type doctorReport struct {
	Schema           string                `json:"schema"`
	OK               bool                  `json:"ok"`
	ServiceScope     string                `json:"service_scope"`
	WorkerGeneration uint64                `json:"worker_generation,omitempty"`
	OSBootID         string                `json:"os_boot_id,omitempty"`
	Availability     *hostservice.Response `json:"availability,omitempty"`
	Connector        *health.Capability    `json:"connector,omitempty"`
	Checks           []doctorCheck         `json:"checks"`
	causes           []error
}

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print JSON")
	stateRoot := flags.String("state-root", "", "Paperboat runtime state directory")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("doctor accepts --json and --state-root only")
	}
	if *stateRoot == "" {
		*stateRoot = os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	}
	if *stateRoot == "" {
		root, err := helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return err
		}
		*stateRoot = root
	}
	report := collectDoctor(ctx, *stateRoot)
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return err
		}
		if !report.OK {
			return runtimeFailure("one or more Paperboat diagnostics failed; run pb doctor to inspect recovery", report.causes...)
		}
		return nil
	}
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(stdout, "%-12s %-28s %s\n", strings.ToUpper(check.State), check.Code, check.Message); err != nil {
			return runtimeFailure("writing Paperboat diagnostics failed", err, runtimeFailure("Paperboat diagnostics failed", report.causes...))
		}
		if check.Recovery != "" {
			if _, err := fmt.Fprintf(stdout, "             Recovery: %s\n", check.Recovery); err != nil {
				return runtimeFailure("writing Paperboat diagnostics failed", err)
			}
		}
	}
	if !report.OK {
		return runtimeFailure("one or more Paperboat diagnostics failed; run pb doctor to inspect recovery", report.causes...)
	}
	return nil
}

func collectDoctor(ctx context.Context, stateRoot string) doctorReport {
	report := doctorReport{Schema: "paperboat.doctor/v1", OK: true, Checks: make([]doctorCheck, 0, 4)}
	add := func(code, state, message, recovery string) {
		report.Checks = append(report.Checks, doctorCheck{Code: code, State: state, Message: message, Recovery: recovery})
		if state == "error" {
			report.OK = false
		}
	}

	scope, err := systemServiceScope(ctx)
	report.ServiceScope = scope
	if err != nil {
		report.causes = append(report.causes, err)
		add("boot_service", "error", "The boot-level worker service is not active.", "Run pb pair again or inspect the system service logs.")
	} else {
		add("boot_service", "ready", "The worker is active as a system boot service.", "")
	}

	var boot struct {
		Schema     string    `json:"schema"`
		OSBootID   string    `json:"os_boot_id"`
		Generation uint64    `json:"generation"`
		StartedAt  time.Time `json:"started_at"`
	}
	if err := decodeStrictFile(filepath.Join(stateRoot, "runtime", "worker-boot.json"), 4096, &boot); err != nil || boot.Schema != "paperboat.worker-boot/v1" || boot.OSBootID == "" || boot.Generation < 1 {
		report.causes = append(report.causes, err)
		add("worker_generation", "error", "Worker boot generation is unavailable.", "Check ownership of the Paperboat runtime state directory, then restart the system service.")
	} else {
		report.WorkerGeneration, report.OSBootID = boot.Generation, boot.OSBootID
		add("worker_generation", "ready", fmt.Sprintf("Worker generation %d is recorded for this OS boot.", boot.Generation), "")
	}

	healthCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	listenAddress := workerListenAddress(stateRoot)
	request, requestErr := http.NewRequestWithContext(healthCtx, http.MethodGet, "http://"+listenAddress+"/healthz", nil)
	var snapshot health.Snapshot
	var healthErr error
	if requestErr != nil {
		healthErr = requestErr
	} else {
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
		healthErr = err
		if response != nil && response.Body != nil {
			if response.StatusCode != http.StatusOK {
				healthErr = runtimeFailure("runtime health request failed", healthErr, runtimeHTTPFailure{status: response.StatusCode})
			} else {
				body, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
				healthErr = runtimeFailureIfAny("reading runtime health failed", healthErr, readErr)
				if readErr == nil && len(body) <= 64<<10 {
					healthErr = runtimeFailureIfAny("invalid runtime health response", healthErr, json.Unmarshal(body, &snapshot))
				} else if len(body) > 64<<10 {
					healthErr = runtimeFailure("runtime health response exceeded its limit", healthErr)
				}
			}
			healthErr = runtimeFailureIfAny("closing runtime health response failed", healthErr, response.Body.Close())
		}
	}
	if healthErr != nil || !snapshot.Live {
		report.causes = append(report.causes, healthErr)
		add("worker_health", "error", "The local host-runtime health endpoint is unavailable.", "Inspect the Paperboat host service logs and restart the service.")
	} else {
		add("worker_health", "ready", "The local host-runtime health endpoint is live.", "")
		if connector, ok := snapshot.Capabilities["edge"]; ok {
			copy := connector
			report.Connector = &copy
			if connector.State == health.Ready {
				add("connector_recovery", "ready", "The outbound connector is ready.", "")
			} else {
				add("connector_recovery", "warning", "The connector is retrying automatically.", "Restore DNS/control-plane/tunnel reachability; recovery is automatic.")
			}
		}
	}

	host, err := hostDiagnostics(ctx)
	if err != nil {
		report.causes = append(report.causes, err)
		add("availability", "error", "The privileged host service is unavailable.", "Inspect and recover the Paperboat host service; then set machine availability to allow-sleep if needed.")
	} else {
		report.Availability = &host
		state := "ready"
		if host.Status == "error" || host.ErrorCode != "" {
			state = "error"
		}
		add("availability", state, fmt.Sprintf("Desired %s version %d; observed %s version %d (%s).", host.DesiredMode, host.DesiredVersion, host.ObservedMode, host.ObservedVersion, host.Status), "Run `pb machine availability <machine> --mode allow-sleep` and wait for it to apply.")
	}
	return report
}

// systemServiceScope checks the native services owned by the unified runtime.
func systemServiceScope(ctx context.Context) (string, error) {
	return systemServiceScopeWithRunner(ctx, runtime.GOOS, os.Getuid(), runSystemServiceCommand)
}

type systemServiceCommandRunner func(context.Context, string, ...string) ([]byte, error)

func systemServiceScopeWithRunner(ctx context.Context, platform string, uid int, run systemServiceCommandRunner) (string, error) {
	if ctx == nil || run == nil || uid < 0 {
		return "system", errors.New("inactive")
	}
	instance := "u" + strconv.Itoa(uid)
	var commands [][]string
	switch platform {
	case "linux":
		commands = append(commands,
			[]string{"/usr/bin/systemctl", "is-active", "paperboat-hostd-" + instance + ".service"},
			[]string{"/usr/bin/systemctl", "is-active", "paperboat-updated-" + instance + ".service"},
		)
		commands = append(commands, []string{"/usr/bin/systemctl", "is-active", "paperboat-runtime-privileged-" + instance + ".service"})
	case "darwin":
		commands = append(commands,
			[]string{"/bin/launchctl", "print", "system/com.pinksaucepasta.paperboat.hostd." + instance},
			[]string{"/bin/launchctl", "print", "system/com.pinksaucepasta.paperboat.updated." + instance},
		)

	default:
		return "system", errors.New("inactive")
	}
	for _, command := range commands {
		output, err := run(ctx, command[0], command[1:]...)
		if err != nil || (platform == "linux" && strings.TrimSpace(string(output)) != "active") || (platform == "darwin" && len(output) == 0) {
			return "system", runtimeFailure("host service is inactive", err)
		}
	}
	return "system", nil
}

func runSystemServiceCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func hostDiagnostics(ctx context.Context) (response hostservice.Response, resultErr error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	connection, err := dialer.DialContext(ctx, "unix", "/var/run/paperboat/host-service.sock")
	if err != nil {
		return hostservice.Response{}, err
	}
	defer func() {
		if closeErr := connection.Close(); closeErr != nil {
			response = hostservice.Response{}
			resultErr = withCleanup("closing host diagnostics connection failed", resultErr, closeErr)
		}
	}()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return hostservice.Response{}, runtimeFailure("setting host diagnostics deadline failed", err)
	}
	if err := json.NewEncoder(connection).Encode(hostservice.Request{Schema: hostservice.ProtocolV1, Operation: "diagnostics"}); err != nil {
		return hostservice.Response{}, err
	}
	closer, ok := connection.(interface{ CloseWrite() error })
	if !ok {
		return hostservice.Response{}, errors.New("invalid host diagnostics connection")
	}
	if err := closer.CloseWrite(); err != nil {
		return hostservice.Response{}, runtimeFailure("closing host diagnostics request failed", err)
	}
	decoder := json.NewDecoder(io.LimitReader(connection, 16<<10))
	decoder.DisallowUnknownFields()
	var extra any
	if err := decoder.Decode(&response); err != nil {
		return hostservice.Response{}, runtimeFailure("invalid host diagnostics", err)
	}
	if err := decoder.Decode(&extra); err != io.EOF {
		return hostservice.Response{}, runtimeFailure("invalid host diagnostics", err)
	}
	if response.Schema != hostservice.ProtocolV1 || response.HostServiceVersion == "" || response.Scope != "system" {
		return hostservice.Response{}, errors.New("invalid host diagnostics")
	}
	return response, nil
}

func workerListenAddress(stateRoot string) string {
	var local struct {
		Schema        string `json:"schema"`
		ListenAddress string `json:"listen_address"`
	}
	if decodeStrictFile(filepath.Join(stateRoot, "runtime", "worker-local.json"), 4096, &local) == nil && local.Schema == "paperboat.worker-local/v1" && strings.HasPrefix(local.ListenAddress, "127.0.0.1:") {
		return local.ListenAddress
	}
	return runtimeport.Primary
}

func decodeStrictFile(path string, limit int64, target any) (resultErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return runtimeFailure("runtime state file is unavailable", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > limit {
		return runtimeFailure("runtime state file is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return runtimeFailure("opening runtime state file failed", err)
	}
	defer func() { resultErr = withCleanup("closing runtime state file failed", resultErr, file.Close()) }()
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return runtimeFailure("reading runtime state file failed", err)
	}
	if int64(len(body)) > limit {
		return runtimeFailure("runtime state file exceeded its limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return runtimeFailure("runtime state file is invalid", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return runtimeFailure("runtime state file is invalid", err)
	}
	return nil
}
