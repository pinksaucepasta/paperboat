//go:build linux || darwin

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

func ConfigureBrowserDomain(ctx context.Context, domain string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	owner := strconv.Itoa(os.Getuid())
	body, err := json.Marshal(BrowserDomainRequest{Owner: owner, Domain: domain})
	if err != nil {
		return err
	}
	args := []string{"--", "/usr/bin/env", "PAPERBOAT_INVOKING_UID=" + owner, executable, "__runtime-service", "browser-domain"}
	command := exec.CommandContext(ctx, "/usr/bin/sudo", args...)
	if os.Geteuid() == 0 {
		command = exec.CommandContext(ctx, "/usr/bin/env", args[2:]...)
	}
	command.Stdin = bytes.NewReader(body)
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("configure trusted local browser domain: %w", err)
	}
	return nil
}
