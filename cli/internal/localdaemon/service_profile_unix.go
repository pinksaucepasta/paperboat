//go:build darwin || linux

package localdaemon

import (
	"fmt"
	"path/filepath"

	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// OtherProfileError is the refusal to install the local daemon service for one profile (config file) over the service
// another profile already runs. The service is one per OS user (one label, one unit, one socket, one machine identity),
// so a second profile that rewrote it would silently replace the first profile's daemon (macOS, 2026-11-11).
type OtherProfileError struct {
	Existing, Requested string
}

func (e *OtherProfileError) Error() string {
	return fmt.Sprintf("Paperboat's background service on this computer already runs for another profile (config %s); this profile (config %s) did not take it over. Use that profile, or run `pb service uninstall` with it first to move the service to this one.", e.Existing, e.Requested)
}

// declaredConfigPath is the --config the installed local daemon service runs with ("" when none is installed or it has
// none).
func declaredConfigPath(config serviceConfig) (string, error) {
	installer, err := newServiceInstaller(config)
	if err != nil {
		return "", err
	}
	return declaredConfigAt(config.Platform, installer.DefinitionPath())
}

func declaredConfigAt(platform, definitionPath string) (string, error) {
	arguments, err := hostservice.DeclaredArguments(platform, definitionPath)
	if err != nil {
		return "", err
	}
	for i := 0; i+1 < len(arguments); i++ {
		if arguments[i] == "--config" {
			return arguments[i+1], nil
		}
	}
	return "", nil
}

// refuseOtherProfile fails when the installed service runs for a different config than the one being installed.
func refuseOtherProfile(config serviceConfig) error {
	if config.ConfigPath == "" {
		return nil
	}
	existing, err := declaredConfigPath(config)
	if err != nil || existing == "" || filepath.Clean(existing) == filepath.Clean(config.ConfigPath) {
		return nil // nothing installed, or it is this profile's; an unreadable definition keeps the install's own checks
	}
	return &OtherProfileError{Existing: existing, Requested: filepath.Clean(config.ConfigPath)}
}

// ServiceProfile is the config path the installed local daemon service runs for ("" when none is installed).
func ServiceProfile(executable string) (string, error) {
	config, err := currentUserServiceConfig(executable)
	if err != nil {
		return "", err
	}
	return declaredConfigPath(config)
}
