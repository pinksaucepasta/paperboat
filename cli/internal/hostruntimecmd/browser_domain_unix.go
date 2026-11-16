//go:build linux || darwin

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	command := sudoCommand(ctx, args...)
	command.Stdin = bytes.NewReader(body)
	command.Stdout = os.Stderr
	reason := &tailBuffer{limit: 2048}
	command.Stderr = io.MultiWriter(os.Stderr, reason)
	if err := command.Run(); err != nil {
		if refused := sudoRefusal("Setting up the trusted local browser domain", err, reason.String()); refused != nil {
			return refused
		}
		return fmt.Errorf("configure trusted local browser domain: %w", err)
	}
	return nil
}
