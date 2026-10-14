//go:build windows

package hostinstall

func secureUpdateState(path string, request Request) error {
	// The updater owns this directory. Enrolled-user RX is appropriate for
	// runtime configuration, but breaks the private eligibility-store contract.
	return ensureWindowsProtectedMachineDirectory(path, request.OwnerSID, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
}
