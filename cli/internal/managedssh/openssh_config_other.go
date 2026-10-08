//go:build !darwin && !linux && !windows

package managedssh

import "errors"

var ErrOpenSSHConfigConflict = errors.New("Paperboat OpenSSH configuration conflicts with existing state")

type OpenSSHConfig struct {
	Home, ProxyCommand, KnownHostsCommand, AgentSocket, IdentityFile string
	OwnerUID                                                         uint32
}
type OpenSSHConfigResult struct{ Changed bool }

func InstallOpenSSHConfig(OpenSSHConfig) (OpenSSHConfigResult, error) {
	err := errors.New("OpenSSH configuration is unsupported on this platform")
	return OpenSSHConfigResult{}, managedSSHFailure("command", err, err)
}
func ValidateOpenSSHConfig(OpenSSHConfig) error {
	err := errors.New("OpenSSH configuration is unsupported on this platform")
	return managedSSHFailure("command", err, err)
}
func ValidateInstalledOpenSSHConfig(string, uint32, string) error {
	err := errors.New("OpenSSH configuration is unsupported on this platform")
	return managedSSHFailure("command", err, err)
}
func UninstallOpenSSHConfig(string, uint32) (OpenSSHConfigResult, error) {
	err := errors.New("OpenSSH configuration is unsupported on this platform")
	return OpenSSHConfigResult{}, managedSSHFailure("command", err, err)
}
