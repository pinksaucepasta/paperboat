package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var managedSSHInclude = regexp.MustCompile(`(?mi)^[ \t]*Include[ \t]+~/\.ssh/paperboat_config\b`)

// requireManagedSSHConfiguration must pass before OpenSSH is started for a Paperboat machine: the proxy and host keys
// live only in the fragment, and without it OpenSSH would connect to whatever answers the machine's name.
func requireManagedSSHConfiguration() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return commandPreparationFailure{step: prepareSSHConfiguration, cause: err}
	}
	main, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err == nil && !managedSSHInclude.Match(main) {
		err = errors.New("the Include line is missing")
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
