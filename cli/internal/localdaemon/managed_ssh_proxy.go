package localdaemon

import "strings"

// managedSSHProxyCommand preserves the OS username through OpenSSH's shell.
func managedSSHProxyCommand(quotedExecutable string) string {
	return quotedExecutable + " __ssh-proxy --host %h --port %p --user \"%r\""
}

// windowsOpenSSHExecutable writes a drive-letter executable path in the one form both ssh flavours on Windows accept
// in the same ~/.ssh config, ProxyCommand and KnownHostsCommand alike. The built-in Microsoft OpenSSH takes any
// drive path; Git for Windows' MSYS ssh refuses C:\ and C:/ for KnownHostsCommand ("path is not absolute") and
// runs a ProxyCommand starting with a drive path through cmd.exe, which mis-parses it. `//?/C:/…` passed every
// check and ran in both (winpool-11, OpenSSH_for_Windows_10.0p2 and MSYS OpenSSH_10.6p1, 2026-11-12). A path that is
// not a drive path is only slashed.
func windowsOpenSSHExecutable(path string) string {
	slashed := strings.ReplaceAll(path, `\`, "/")
	if len(slashed) >= 3 && slashed[1] == ':' && slashed[2] == '/' && (slashed[0] >= 'A' && slashed[0] <= 'Z' || slashed[0] >= 'a' && slashed[0] <= 'z') {
		return "//?/" + slashed
	}
	return slashed
}
