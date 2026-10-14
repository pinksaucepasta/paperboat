//go:build windows

package daemoncmd

import (
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

func runWindowsSSHService(instance string) error {
	config, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return err
	}
	layout, err := hostinstall.WindowsLayoutForInstance(instance)
	if err != nil {
		return err
	}
	root, err := hostinstall.WindowsInstanceRoot(instance)
	if err != nil {
		return err
	}
	ssh := windowsopenssh.DefaultConfig(nil)
	name := "PaperboatSshd-" + instance
	ssh.ServiceName = name
	ssh.OwnerSID = config.OwnerSID
	ssh.StateRoot = filepath.Join(root, "ssh")
	ssh.ServiceExecutable = layout.Binary
	return windowsopenssh.RunServiceHost(name, filepath.Join(ssh.InstallRoot, "sshd.exe"), filepath.Join(ssh.StateRoot, "sshd_config"), config.OwnerSID)
}
