//go:build windows

package hostinstall

import "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"

// WindowsConfigServiceDefinition derives every privileged service field from
// the protected installation belonging to the elevation request owner.
func WindowsConfigServiceDefinition(ownerSID string) (service.Config, error) {
	instance, err := WindowsInstanceForSID(ownerSID)
	if err != nil {
		return service.Config{}, err
	}
	install, err := LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return service.Config{}, err
	}
	return windowsConfigServiceDefinition(install, ownerSID)
}

func windowsConfigServiceDefinition(install WindowsRuntimeConfig, ownerSID string) (service.Config, error) {
	instance, err := WindowsInstanceForSID(ownerSID)
	if err != nil {
		return service.Config{}, err
	}
	if install.Instance != instance || install.OwnerSID != ownerSID || !install.Committed || install.MachineID == "" {
		return service.Config{}, ErrInvalidRequest
	}
	layout, err := WindowsLayoutForInstance(instance)
	if err != nil {
		return service.Config{}, err
	}
	return windowsConfigServiceDefinitionForLayout(layout)
}

func windowsConfigServiceDefinitionForLayout(layout service.Layout) (service.Config, error) {
	instance := layout.Instance
	root, err := WindowsInstanceRoot(instance)
	if err != nil {
		return service.Config{}, err
	}
	return service.Config{Platform: "windows", Kind: service.ConfigKind, Instance: instance,
		ConfigRoot: root, Executable: layout.Binary, User: "Paperboat", Group: "Paperboat",
		Arguments:  []string{"daemon", "__runtime-config", "--instance", instance},
		Controller: service.WindowsController{}}, nil
}
