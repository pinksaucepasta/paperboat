//go:build !windows

package hostruntimecmd

// InheritedInstallServer is only resolved on Windows; elsewhere it reports that it cannot tell.
func InheritedInstallServer() (string, bool) { return "", false }
