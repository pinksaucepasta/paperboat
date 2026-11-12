package main

import (
	"errors"
	"testing"
)

func TestGitForWindowsSSHAdvice(t *testing.T) {
	found := func(string) (string, error) { return "git", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	for name, c := range map[string]struct {
		goos     string
		lookPath func(string) (string, error)
		setting  string
		want     string
	}{
		"Windows, Git installed, core.sshCommand unset": {"windows", found, "", gitForWindowsSSHFix},
		"Windows, whitespace only":                      {"windows", found, " \n", gitForWindowsSSHFix},
		"Windows, core.sshCommand already set":          {"windows", found, "C:/Windows/System32/OpenSSH/ssh.exe\n", ""},
		"Windows, no Git":                               {"windows", missing, "", ""},
		"Linux never advises":                           {"linux", found, "", ""},
		"macOS never advises":                           {"darwin", found, "", ""},
	} {
		if got := gitForWindowsSSHAdvice(c.goos, c.lookPath, func() string { return c.setting }); got != c.want {
			t.Errorf("%s: advice = %q, want %q", name, got, c.want)
		}
	}
	if gitForWindowsSSHFix != "git config --global core.sshCommand C:/Windows/System32/OpenSSH/ssh.exe" {
		t.Errorf("the fix line changed: %q", gitForWindowsSSHFix)
	}
}
