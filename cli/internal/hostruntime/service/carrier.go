package service

import (
	"context"
	"strconv"
)

// generationalUnit reports the roles that run one native unit per installed generation on Linux and
// macOS, like session owners. An update starts and readies the new generation's units before the old
// generation retires, so nothing is refused or paused (make-before-break). Prior art: Teleport graceful
// restart (lib/service/signals.go: the new process serves before the old one shuts down and drains its
// connections) and nginx binary upgrade (~/ref/nginx/src/os/unix/ngx_process_cycle.c: old workers
// finish their connections). Separate units mean the service manager never kills the old process.
func generationalUnit(config Config) bool {
	return (config.Kind == CarrierKind || config.Kind == HostdKind) && config.Platform != "windows"
}

func validateCarrierConfig(config Config) error {
	if config.Kind != CarrierKind {
		return nil
	}
	if !safeInstance(config.Instance) {
		return ErrInvalidDefinition
	}
	want := []string{"daemon", "__runtime-carrier"}
	if config.Platform == "windows" {
		if !validWindowsSessionOwnerInstance(config.Instance) {
			return ErrInvalidDefinition
		}
		want = append(want, "--instance", config.Instance)
	} else {
		want = append(want, "--generation", config.Generation)
	}
	if len(config.Arguments) != len(want) {
		return ErrInvalidDefinition
	}
	for i := range want {
		if config.Arguments[i] != want[i] {
			return ErrInvalidDefinition
		}
	}
	return nil
}

// CarrierController addresses one native carrier generation of the enrolled account. On Linux and
// macOS each generation has its own unit, so a new carrier serves before the old one retires.
func CarrierController(platform, instance, generation string, enrolledUID int, runner Runner) (Controller, error) {
	if !safeInstance(instance) || enrolledUID < 0 || platform != "windows" && !validSessionOwnerGeneration(generation) {
		return nil, ErrInvalidDefinition
	}
	switch platform {
	case "linux":
		if runner == nil {
			return nil, ErrInvalidDefinition
		}
		return SystemdController{Runner: runner, Unit: "paperboat-carrier-" + instance + "-" + generation + ".service"}, nil
	case "darwin":
		if runner == nil {
			return nil, ErrInvalidDefinition
		}
		return LaunchdController{Runner: runner, UID: enrolledUID, Label: CarrierLabel + "." + instance + "." + generation}, nil
	case "windows":
		if !validWindowsSessionOwnerInstance(instance) || generation != "" {
			return nil, ErrInvalidDefinition
		}
		return windowsSessionOwnerController()
	default:
		return nil, ErrUnsupportedPlatform
	}
}

// HostdController addresses one hostd generation of the enrolled account on Linux and macOS.
func HostdController(platform, generation string, enrolledUID int, runner Runner) (Controller, error) {
	if runner == nil || enrolledUID < 0 || !validSessionOwnerGeneration(generation) {
		return nil, ErrInvalidDefinition
	}
	instance := "u" + strconv.Itoa(enrolledUID)
	switch platform {
	case "linux":
		return SystemdController{Runner: runner, Unit: "paperboat-hostd-" + instance + "-" + generation + ".service"}, nil
	case "darwin":
		return LaunchdController{Runner: runner, UID: enrolledUID, Label: HostdLabel + "." + instance + "." + generation}, nil
	default:
		return nil, ErrUnsupportedPlatform
	}
}

// QuitCarrier asks a retired carrier generation to shut down gracefully: it stops admitting new
// streams, keeps serving open ones up to runtimewire.DrainLimit, then exits 0 and stays
// down. SIGQUIT is the graceful-shutdown signal of both nginx and Teleport (lib/service/signals.go).
func QuitCarrier(ctx context.Context, controller Controller) error {
	switch c := controller.(type) {
	case SystemdController:
		if c.Runner == nil {
			return ErrInvalidDefinition
		}
		return c.Runner.Run(ctx, "systemctl", "kill", "--kill-whom=main", "--signal=SIGQUIT", c.unit())
	case LaunchdController:
		if c.Runner == nil {
			return ErrInvalidDefinition
		}
		return c.Runner.Run(ctx, "launchctl", "kill", "SIGQUIT", c.service())
	default:
		return ErrUnsupportedPlatform
	}
}

// WindowsCarrierName is the single SCM identity for an enrolled account.
func WindowsCarrierName(instance string) (string, error) {
	if !validWindowsSessionOwnerInstance(instance) {
		return "", ErrInvalidDefinition
	}
	return "PaperboatCarrier-" + instance, nil
}
