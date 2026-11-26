//go:build !windows

package main

// Unix uninstall takes the activation lock and retires the activation itself (hostinstall install_uninstall_unix.go).
func platformUpdateActivating() (bool, error) { return false, nil }
