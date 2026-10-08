//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func runPurgeCommand(ctx context.Context, args []string, _ io.Reader, _, _ io.Writer) error {
	if len(args) != 0 {
		return errors.New("purge accepts no arguments")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "/usr/bin/sudo", "--", "/usr/bin/env", "PAPERBOAT_INVOKING_UID="+strconv.Itoa(os.Getuid()), executable, "__runtime-service", "purge")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return errors.Join(err, errors.New(stderr.String()))
	}
	return nil
}

func purgeSystemInstallation(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("complete uninstall requires administrator approval")
	}
	plan, err := newUnixPurgePlan(runtime.GOOS, invokingServiceUID())
	if err != nil {
		return err
	}
	return applyUnixPurgePlan(ctx, plan,
		func(commandCtx context.Context, executable string, arguments ...string) error {
			return exec.CommandContext(commandCtx, executable, arguments...).Run()
		}, os.RemoveAll)
}

// unixPurgePlan is the complete set of Paperboat-owned native declarations and
// payload roots removed by an explicit privileged purge. Keeping the plan
// separate from command execution makes both supported service managers
// auditable without requiring a live systemd or launchd host in unit tests.
type unixPurgePlan struct {
	platform           string
	systemdUnits       []string
	systemdDefinitions []string
	launchdLabels      []string
	launchdDefinitions []string
	payloadPaths       []string
}

func newUnixPurgePlan(platform string, uid int) (unixPurgePlan, error) {
	if uid < 0 {
		return unixPurgePlan{}, errors.New("invalid enrolled user")
	}
	instance := "u" + strconv.Itoa(uid)
	switch platform {
	case "linux":
		units := []string{
			"paperboat-runtime-privileged-" + instance + ".service",
			"paperboat-hostd-" + instance + ".service",
			"paperboat-updated-" + instance + ".service",
		}
		return unixPurgePlan{
			platform:           platform,
			systemdUnits:       units,
			systemdDefinitions: appendSystemdDefinitions(units),
			payloadPaths: []string{
				filepath.Join("/usr/local/libexec/paperboat/users", instance),
				filepath.Join("/var/lib/paperboat-installer/users", instance),
				filepath.Join("/var/lib/paperboat/users", instance),
				"/var/lib/paperboat-updated-" + instance,
				"/var/run/paperboat-hostd-" + instance,
				"/var/run/paperboat-updated-" + instance,
			},
		}, nil
	case "darwin":
		labels := []string{service.HostLabel + "." + instance, service.HostdLabel + "." + instance, service.UpdaterLabel + "." + instance}
		definitions := make([]string, 0, len(labels))
		for _, label := range labels {
			definitions = append(definitions, filepath.Join("/Library", "LaunchDaemons", label+".plist"))
		}
		return unixPurgePlan{
			platform:           platform,
			launchdLabels:      labels,
			launchdDefinitions: definitions,
			payloadPaths: []string{
				filepath.Join("/Library/PrivilegedHelperTools/Paperboat/users", instance),
				filepath.Join("/Library/Application Support/Paperboat/users", instance),
				"/var/run/paperboat-hostd-" + instance,
				"/var/run/paperboat-updated-" + instance,
			},
		}, nil
	default:
		return unixPurgePlan{}, errors.New("unsupported purge platform")
	}
}

func appendSystemdDefinitions(units []string) []string {
	definitions := make([]string, 0, len(units)+1)
	for _, unit := range units {
		definitions = append(definitions, filepath.Join("/etc", "systemd", "system", unit))
	}
	definitions = append(definitions, "/etc/systemd/system/paperboat-helper.service.d")
	return definitions
}

func applyUnixPurgePlan(ctx context.Context, plan unixPurgePlan, run func(context.Context, string, ...string) error, remove func(string) error) error {
	if ctx == nil || run == nil || remove == nil {
		return errors.New("invalid purge operation")
	}
	if plan.platform == "linux" {
		for _, action := range []string{"stop", "disable"} {
			arguments := append([]string{action}, plan.systemdUnits...)
			_ = run(ctx, "/usr/bin/systemctl", arguments...)
		}
	} else if plan.platform == "darwin" {
		for _, label := range plan.launchdLabels {
			_ = run(ctx, "/bin/launchctl", "bootout", "system/"+label)
		}
	}

	var result error
	definitions := plan.systemdDefinitions
	if plan.platform == "darwin" {
		definitions = plan.launchdDefinitions
	}
	for _, path := range append(definitions, plan.payloadPaths...) {
		result = errors.Join(result, remove(path))
	}
	if plan.platform == "linux" {
		_ = run(ctx, "/usr/bin/systemctl", "daemon-reload")
		arguments := append([]string{"reset-failed"}, plan.systemdUnits...)
		_ = run(ctx, "/usr/bin/systemctl", arguments...)
	}
	return result
}
