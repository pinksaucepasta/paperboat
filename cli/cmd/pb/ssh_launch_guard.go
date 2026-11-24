package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
)

var managedSSHInclude = regexp.MustCompile(`(?mi)^[ \t]*Include[ \t]+~/\.ssh/paperboat_config(?:\s|$)`)
var disabledManagedSSHInclude = regexp.MustCompile(`(?mi)^[ \t]*#[# \t]*Include[ \t]+~/\.ssh/paperboat_config(?:\s|$)`)

func managedSSHIncludeError(main []byte) error {
	switch {
	case managedSSHInclude.Match(main[:managedssh.OpenSSHFirstBlockOffset(main)]):
		return nil
	case managedSSHInclude.Match(main):
		return errors.New("the Include line is inside a Host or Match block and applies only there")
	case disabledManagedSSHInclude.Match(main):
		return errors.New("the Include line is commented out")
	}
	return errors.New("the Include line is missing")
}

// doctorSSHIncludeCheck reports an installed managed SSH configuration that ~/.ssh/config does not apply to every host.
func doctorSSHIncludeCheck(context.Context) doctorpkg.Check {
	check := doctorpkg.Check{Category: "ssh", Code: "managed_ssh_include", Status: doctorpkg.StatusPass, Summary: "~/.ssh/config includes Paperboat's SSH configuration."}
	home, err := os.UserHomeDir()
	if err == nil {
		_, err = os.Lstat(filepath.Join(home, ".ssh", "paperboat_config"))
	}
	if errors.Is(err, os.ErrNotExist) {
		check.Summary = "Paperboat's SSH configuration is not installed on this computer."
		return check
	}
	var main []byte
	if err == nil {
		main, err = os.ReadFile(filepath.Join(home, ".ssh", "config"))
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		check.Status, check.Summary = doctorpkg.StatusUnavailable, "~/.ssh/config could not be read."
		return check
	}
	if err := managedSSHIncludeError(main); err != nil {
		check.Status = doctorpkg.StatusWarning
		check.Summary = "SSH to your machines does not work from this computer: " + err.Error() + " in ~/.ssh/config."
		check.Recovery = "Put `Include ~/.ssh/paperboat_config # paperboat-managed-ssh-include-v1` above the first Host or Match block. While its service runs, Paperboat moves a line inside a block there itself; a line you commented out or removed stays as it is."
	}
	return check
}

// requireManagedSSHConfiguration must pass before OpenSSH is started for a Paperboat machine: the proxy and host keys
// live only in the fragment, and without it OpenSSH would connect to whatever answers the machine's name.
func requireManagedSSHConfiguration() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return commandPreparationFailure{step: prepareSSHConfiguration, cause: err}
	}
	main, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err == nil {
		err = managedSSHIncludeError(main)
	}
	if err == nil {
		var fragment []byte
		fragment, err = os.ReadFile(filepath.Join(home, ".ssh", "paperboat_config"))
		if err == nil && !strings.Contains(string(fragment), "__ssh-proxy") {
			err = errors.New("the fragment has no proxy")
		}
	}
	if err != nil {
		return commandPreparationFailure{step: prepareSSHConfiguration, cause: err}
	}
	return nil
}
