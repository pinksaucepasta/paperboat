package service

import (
	"context"
	"strconv"
	"strings"
)

// generationalUnit reports the roles that run one native unit or service per installed generation, like session
// owners: the carrier and hostd. An update starts and readies the new generation's units before the old
// generation retires, so nothing is refused or paused (make-before-break). Prior art: Teleport graceful
// restart (lib/service/signals.go: the new process serves before the old one shuts down and drains its
// connections) and nginx binary upgrade (~/ref/nginx/src/os/unix/ngx_process_cycle.c: old workers
// finish their connections). Separate units mean the service manager never kills the old process.
func generationalUnit(config Config) bool {
	return config.Kind == CarrierKind || config.Kind == HostdKind
}

// RetiredGenerationLogPath is where launchd writes a generation's unit output (renderLaunchd's StandardOutPath).
func RetiredGenerationLogPath(label string) string { return "/var/log/" + label + ".log" }

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
	}
	want = append(want, "--generation", config.Generation)
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
	if !safeInstance(instance) || enrolledUID < 0 || !validSessionOwnerGeneration(generation) {
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
		if !validWindowsSessionOwnerInstance(instance) {
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

// QuitCarrier asks a carrier generation that is no longer active to exit 0 and stay down.
// SIGQUIT is the graceful-shutdown signal of both nginx and Teleport (lib/service/signals.go).
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

// WindowsCarrierName is the SCM identity of one carrier generation of an enrolled account, like the session
// owner's: an update's candidate runs beside the serving generation.
func WindowsCarrierName(instance, generation string) (string, error) {
	if !validWindowsSessionOwnerInstance(instance) || !validSessionOwnerGeneration(generation) {
		return "", ErrInvalidDefinition
	}
	return "PaperboatCarrier-" + instance + "-" + generation, nil
}

// WindowsHostdName is the SCM identity of one hostd generation of an enrolled account: an update's new hostd runs
// beside the serving one and takes its listeners over (W21 Part B).
func WindowsHostdName(instance, generation string) (string, error) {
	if !validWindowsSessionOwnerInstance(instance) || !validSessionOwnerGeneration(generation) {
		return "", ErrInvalidDefinition
	}
	return "PaperboatHostd-" + instance + "-" + generation, nil
}

// windowsCarrierNameParts splits a carrier generation's SCM name (WindowsCarrierName).
func windowsCarrierNameParts(name string) (instance, generation string, valid bool) {
	return windowsGenerationNameParts("PaperboatCarrier-", name)
}

// windowsHostdNameParts splits a hostd generation's SCM name (WindowsHostdName).
func windowsHostdNameParts(name string) (instance, generation string, valid bool) {
	return windowsGenerationNameParts("PaperboatHostd-", name)
}

func windowsGenerationNameParts(prefix, name string) (instance, generation string, valid bool) {
	if len(name) < len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) {
		return "", "", false
	}
	suffix := name[len(prefix):]
	if len(suffix) < 34 || suffix[len(suffix)-33] != '-' {
		return "", "", false
	}
	instance, generation = suffix[:len(suffix)-33], suffix[len(suffix)-32:]
	return instance, generation, validWindowsSessionOwnerInstance(instance) && validSessionOwnerGeneration(generation)
}
