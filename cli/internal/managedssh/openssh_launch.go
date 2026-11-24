package managedssh

import (
	"context"
	"errors"
	"strings"
	"time"
)

// replacedOpenSSHFragment keeps the fragment an install replaced when pb had not written it.
const replacedOpenSSHFragment = "paperboat_config.replaced"

// ErrOpenSSHLaunchUnpinned is an OpenSSH invocation that would not use Paperboat's transport and host-key pinning.
var ErrOpenSSHLaunchUnpinned = errors.New("OpenSSH would not use Paperboat's connection and host keys")

// OpenSSHLaunch is an OpenSSH invocation to one managed host, checked by VerifyOpenSSHLaunch.
type OpenSSHLaunch struct {
	Home        string
	OwnerUID    uint32
	AgentSocket string
	Domain      string
	Host        string
	Executable  string
	Arguments   []string
	Environment []string
}

// VerifyOpenSSHLaunch accepts an invocation only when the installed fragment is intact and OpenSSH itself (ssh -G,
// the same arguments and environment) resolves the host to that fragment's transport and host-key trust. The second
// half covers what a file check cannot see: -F, -o, -J, another Host block, or a HOME that OpenSSH does not read.
func VerifyOpenSSHLaunch(ctx context.Context, launch OpenSSHLaunch) error {
	owned, err := readInstalledOpenSSHConfig(launch.Home, launch.OwnerUID, launch.AgentSocket, launch.Domain)
	if err != nil {
		return err
	}
	executable, err := resolveOpenSSHExecutable(launch.Executable)
	if err != nil {
		return errors.Join(ErrOpenSSHUnavailable, err)
	}
	// -G runs the fragment's Match exec, which may fetch a certificate.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := runOpenSSHProbeEnv(ctx, launch.Environment, executable, append([]string{"-G"}, launch.Arguments...)...)
	if err != nil {
		return errors.Join(ErrOpenSSHLaunchUnpinned, err)
	}
	effective := make(map[string]string)
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if key, value, found := strings.Cut(line, " "); found {
			effective[strings.ToLower(key)] = strings.TrimSpace(value)
		}
	}
	strict := effective["stricthostkeychecking"]
	alias := effective["hostkeyalias"]
	dns := effective["verifyhostkeydns"]
	if !strings.EqualFold(effective["hostname"], launch.Host) ||
		effective["proxycommand"] != owned.ProxyCommand || effective["knownhostscommand"] != owned.KnownHostsCommand ||
		strict != "true" && strict != "yes" || alias != "" && alias != "none" || dns != "" && dns != "false" && dns != "no" ||
		effective["userknownhostsfile"] != "none" || effective["globalknownhostsfile"] != "none" {
		return ErrOpenSSHLaunchUnpinned
	}
	return nil
}

// ResolveOpenSSH is the OpenSSH client pb starts and checks: the trusted native one on Windows, else the one on PATH.
func ResolveOpenSSH() (string, error) { return resolveOpenSSHExecutable("ssh") }
