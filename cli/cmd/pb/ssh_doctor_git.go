package main

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// gitForWindowsSSHFix is the one line that lets Git (clone, push, fetch over ssh) use the built-in OpenSSH, which
// reaches the Paperboat managed agent; Git for Windows' own MSYS ssh cannot (its agent is a Windows named pipe).
const gitForWindowsSSHFix = "git config --global core.sshCommand C:/Windows/System32/OpenSSH/ssh.exe"

// gitForWindowsSSHAdvice returns gitForWindowsSSHFix when this is Windows, Git is installed and Git is not yet
// pointed at another ssh. Anything else returns "".
func gitForWindowsSSHAdvice(goos string, lookPath func(string) (string, error), coreSSHCommand func() string) string {
	if goos != "windows" {
		return ""
	}
	if _, err := lookPath("git"); err != nil {
		return ""
	}
	if strings.TrimSpace(coreSSHCommand()) != "" {
		return ""
	}
	return gitForWindowsSSHFix
}

// gitCoreSSHCommand is the user's global core.sshCommand ("" when unset or unreadable).
func gitCoreSSHCommand() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "config", "--global", "--get", "core.sshCommand").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func sshDoctorGitAdvice() string {
	return gitForWindowsSSHAdvice(runtime.GOOS, exec.LookPath, gitCoreSSHCommand)
}
