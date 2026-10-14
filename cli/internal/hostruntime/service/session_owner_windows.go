//go:build windows

package service

func windowsSessionOwnerController() (Controller, error) { return WindowsController{}, nil }

// WindowsSessionOwnerName retains the account instance separately from the
// bounded generation suffix used by its independent SCM declaration.
func WindowsSessionOwnerName(instance, generation string) (string, error) {
	if !validWindowsSessionOwnerInstance(instance) || !validSessionOwnerGeneration(generation) {
		return "", ErrInvalidDefinition
	}
	return "PaperboatSessionOwner-" + instance + "-" + generation, nil
}

func windowsConfiguredServiceName(config Config) string {
	if config.Kind == SessionOwnerKind {
		name, _ := WindowsSessionOwnerName(config.Instance, config.SessionOwnerGeneration)
		return name
	}
	return windowsServiceName(config.Kind, config.Instance)
}

func OwnedWindowsSessionOwnerExecutable(instance, generation string) (WindowsExecutableIdentity, error) {
	if _, err := WindowsSessionOwnerName(instance, generation); err != nil {
		return WindowsExecutableIdentity{}, err
	}
	return OwnedWindowsRoleExecutable(SessionOwnerKind, instance+"-"+generation)
}
func VerifyOwnedWindowsSessionOwnerExecutable(instance, generation, path string) (WindowsExecutableIdentity, error) {
	if _, err := WindowsSessionOwnerName(instance, generation); err != nil {
		return WindowsExecutableIdentity{}, err
	}
	return VerifyOwnedWindowsRoleExecutable(SessionOwnerKind, instance+"-"+generation, path)
}
