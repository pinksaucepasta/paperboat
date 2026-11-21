package main

import doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"

// doctorManagedSSHCheck reports whether an enrolled Windows machine has its Paperboat-managed sshd service.
// Install continues without it when OpenSSH cannot be obtained in time; this is where the user learns of it.
func doctorManagedSSHCheck(enrolled bool, enrolledErr error, serviceState string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "managed_ssh"}
	switch {
	case enrolledErr != nil || serviceState == "unavailable" || serviceState == "invalid":
		check.Status, check.Summary = doctorpkg.StatusUnavailable, "Whether Paperboat's managed SSH service is installed could not be checked."
		check.Recovery = "Run pb doctor again."
	case !enrolled:
		check.Status, check.Summary = doctorpkg.StatusPass, "This machine is not enrolled; managed SSH is not needed."
	case serviceState == "not_installed":
		check.Status, check.Summary = doctorpkg.StatusFail, "Paperboat's managed SSH service (PaperboatSshd) is missing: SSH to this machine will not work."
		check.Recovery = "Run pb doctor --repair to install it."
	case serviceState == "active":
		check.Status, check.Summary = doctorpkg.StatusPass, "Paperboat's managed SSH service is running."
	default:
		check.Status, check.Summary = doctorpkg.StatusWarning, "Paperboat's managed SSH service is installed but not running."
		check.Recovery = "Run pb doctor --repair."
	}
	return check
}

// managedSSHInstallWarning is what install, setup and pair print when the doctor check finds managed SSH missing.
func managedSSHInstallWarning(check doctorpkg.Check) []string {
	if check.Status != doctorpkg.StatusFail {
		return nil
	}
	return []string{check.Summary + " Recovery: " + check.Recovery}
}
