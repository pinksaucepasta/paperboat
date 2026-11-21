//go:build !windows

package main

// Linux and macOS install no managed sshd of their own.
func managedSSHInstallWarnings() []string { return nil }
