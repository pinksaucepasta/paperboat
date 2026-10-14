package service

import "strings"

func validSessionOwnerGeneration(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validateSessionOwnerConfig(config Config) error {
	if config.Kind != SessionOwnerKind {
		if config.SessionOwnerGeneration != "" {
			return ErrInvalidDefinition
		}
		return nil
	}
	if !safeInstance(config.Instance) || !validSessionOwnerGeneration(config.SessionOwnerGeneration) {
		return ErrInvalidDefinition
	}
	want := []string{"daemon", "__runtime-session-owner", "--generation", config.SessionOwnerGeneration}
	if config.Platform == "windows" {
		want = append(want, "--instance", config.Instance)
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

// SessionOwnerController addresses only an independent generation of the
// enrolled account, using the same native installation and removal lifecycle.
func SessionOwnerController(platform, instance, generation string, enrolledUID int, runner Runner) (Controller, error) {
	if !safeInstance(instance) || !validSessionOwnerGeneration(generation) || enrolledUID < 0 {
		return nil, ErrInvalidDefinition
	}
	switch platform {
	case "linux":
		if runner == nil {
			return nil, ErrInvalidDefinition
		}
		return SystemdController{Runner: runner, Unit: "paperboat-session-owner-" + instance + "-" + generation + ".service"}, nil
	case "darwin":
		if runner == nil {
			return nil, ErrInvalidDefinition
		}
		return LaunchdController{Runner: runner, UID: enrolledUID, Label: SessionOwnerLabel + "." + instance + "." + generation}, nil
	case "windows":
		return windowsSessionOwnerController()
	default:
		return nil, ErrUnsupportedPlatform
	}
}

func sessionOwnerNameParts(name string) (string, string, bool) {
	const prefix = "PaperboatSessionOwner-"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if len(suffix) < 34 {
		return "", "", false
	}
	split := len(suffix) - 33
	instance, generation := suffix[:split], suffix[split+1:]
	return instance, generation, suffix[split] == '-' && validWindowsSessionOwnerInstance(instance) && validSessionOwnerGeneration(generation)
}

func validWindowsSessionOwnerInstance(instance string) bool {
	if len(instance) != 25 || instance[0] != 'u' {
		return false
	}
	for _, c := range instance[1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
