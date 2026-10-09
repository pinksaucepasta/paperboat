package localdaemon

// managedSSHProxyCommand preserves the OS username through OpenSSH's shell.
func managedSSHProxyCommand(quotedExecutable string) string {
	return quotedExecutable + " __ssh-proxy --host %h --port %p --user \"%r\""
}
