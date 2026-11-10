//go:build !windows

package windowsopenssh

import "os"

func platformOwnerSID() string { return "" }

func platformSSHComputerName() (string, error) { return os.Hostname() }
