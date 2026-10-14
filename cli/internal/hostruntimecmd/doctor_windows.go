//go:build windows

package hostruntimecmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

type doctorCheck struct {
	Code     string `json:"code"`
	State    string `json:"state"`
	Message  string `json:"message"`
	Recovery string `json:"recovery,omitempty"`
}

type doctorReport struct {
	Schema       string                `json:"schema"`
	OK           bool                  `json:"ok"`
	ServiceScope string                `json:"service_scope"`
	Availability *hostservice.Response `json:"availability,omitempty"`
	Checks       []doctorCheck         `json:"checks"`
	causes       []error
}

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print JSON")
	repair := flags.Bool("repair", false, "repair Paperboat-owned Windows dependencies")
	_ = flags.String("state-root", "", "Paperboat runtime state directory")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("doctor accepts --json, --repair, and --state-root only")
	}
	if *repair {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if err := elevation.RunRuntimeService(ctx, executable, elevation.ActionRepair, nil); err != nil {
			return err
		}
	}
	report := collectWindowsDoctor(ctx)
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return err
		}
	} else {
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
	}
	if !report.OK {
		return runtimeFailure("one or more Paperboat diagnostics failed; run pb doctor to inspect recovery", report.causes...)
	}
	return nil
}

func collectWindowsDoctor(ctx context.Context) doctorReport {
	report := doctorReport{Schema: "paperboat.doctor/v1", OK: true, ServiceScope: "system", Checks: make([]doctorCheck, 0, 3)}
	add := func(code, state, message, recovery string) {
		report.Checks = append(report.Checks, doctorCheck{Code: code, State: state, Message: message, Recovery: recovery})
		if state == "error" {
			report.OK = false
		}
	}
	instance, instanceErr := currentWindowsRuntimeInstance()
	if instanceErr != nil {
		add("installation", "error", "The current user's protected Windows installation is unavailable.", "Run pb doctor --repair to restore the Paperboat installation.")
		return report
	}
	sshConfig := windowsopenssh.DefaultConfig(nil)
	sshConfig.OwnerSID = instance.config.OwnerSID
	sshConfig = windowsOpenSSHBootstrapConfig(sshConfig, instance.name)
	if err := windowsopenssh.CheckFirewallOwnership(ctx, sshConfig); err != nil {
		report.causes = append(report.causes, err)
		add("openssh_firewall", "error", "Paperboat OpenSSH firewall ownership is missing or unsafe.", "Run pb doctor --repair from an elevated prompt.")
	} else {
		add("openssh_firewall", "ready", "Paperboat owns no public SSH exposure.", "")
	}

	host, err := windowsHostDiagnostics(ctx)
	if err != nil {
		report.causes = append(report.causes, err)
		add("availability", "error", "The privileged Windows host service is unavailable.", "Run pb doctor --repair or inspect the Paperboat Windows service logs.")
	} else {
		report.Availability = &host
		state := "ready"
		if host.Status == "error" || host.ErrorCode != "" {
			state = "error"
		}
		add("availability", state, fmt.Sprintf("Desired %s version %d; observed %s version %d (%s).", host.DesiredMode, host.DesiredVersion, host.ObservedMode, host.ObservedVersion, host.Status), "Run `pb machine availability <machine> --mode allow-sleep` and wait for it to apply.")
	}

	client, err := windowsHostdClient()
	if err != nil {
		report.causes = append(report.causes, err)
		add("hostd_lifecycle", "error", "The Windows host-runtime control pipe is unavailable.", "Run pb doctor --repair to restore the Paperboat host supervisor.")
		return report
	}
	statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, err := client.Active(statusCtx)
	if err != nil {
		report.causes = append(report.causes, err)
		add("hostd_lifecycle", "error", "The Windows host supervisor did not answer its authenticated control pipe.", "Run pb doctor --repair to restart the Paperboat host supervisor.")
		return report
	}
	if status.State != hostdproto.StateActive {
		add("runtime_worker", "error", "The Windows runtime worker is not active.", "Run pb doctor --repair to restore the Paperboat runtime worker.")
		return report
	}
	add("hostd_lifecycle", "ready", "The Windows host supervisor is active.", "")
	add("runtime_worker", "ready", fmt.Sprintf("The Windows runtime worker is active at fence epoch %d.", status.Epoch), "")
	return report
}

func windowsHostDiagnostics(ctx context.Context) (response hostservice.Response, resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timed, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	instance, err := currentWindowsRuntimeInstance()
	if err != nil {
		return hostservice.Response{}, err
	}
	socket, err := hostservice.WindowsSocketPath(instance.name)
	if err != nil {
		return hostservice.Response{}, err
	}
	connection, err := winio.DialPipeContext(timed, socket)
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
		return hostservice.Response{}, errors.New("invalid Windows host-service diagnostics")
	}
	return response, nil
}
