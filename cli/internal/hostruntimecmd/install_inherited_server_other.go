//go:build !windows && !darwin && !linux

package hostruntimecmd

// InheritedInstallServer is resolved on Windows, Linux and macOS; elsewhere it reports that it cannot tell.
func InheritedInstallServer() (string, bool) { return "", false }
