package service

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

// CarrierController addresses one stable native carrier for the enrolled
// account. Callers must fence admission and drain flows before replacement;
// unlike session owners, carriers must never overlap for an account identity.
func CarrierController(platform, instance string, enrolledUID int, runner Runner) (Controller, error) {
	if !safeInstance(instance) || enrolledUID < 0 {
		return nil, ErrInvalidDefinition
	}
	switch platform {
	case "linux":
		if runner == nil {
			return nil, ErrInvalidDefinition
		}
		return SystemdController{Runner: runner, Unit: "paperboat-carrier-" + instance + ".service"}, nil
	case "darwin":
		if runner == nil {
			return nil, ErrInvalidDefinition
		}
		return LaunchdController{Runner: runner, UID: enrolledUID, Label: CarrierLabel + "." + instance}, nil
	case "windows":
		if !validWindowsSessionOwnerInstance(instance) {
			return nil, ErrInvalidDefinition
		}
		return windowsSessionOwnerController()
	default:
		return nil, ErrUnsupportedPlatform
	}
}

// WindowsCarrierName is the single SCM identity for an enrolled account.
func WindowsCarrierName(instance string) (string, error) {
	if !validWindowsSessionOwnerInstance(instance) {
		return "", ErrInvalidDefinition
	}
	return "PaperboatCarrier-" + instance, nil
}
