//go:build windows

package hostruntimecmd

import "testing"

// winpool, 2026-11-10: the local daemon's service could not read its installation and exited before the service
// dispatcher. SCM logged "did not respond to the start request", the install timed out after 30 s, and the cause was
// written nowhere.
func TestAServiceThatCannotReadItsInstallationLeavesTheCauseInItsLog(t *testing.T) {
	var names []string
	var causes []error
	previous := recordServiceEntryFailure
	t.Cleanup(func() { recordServiceEntryFailure = previous })
	recordServiceEntryFailure = func(name string, err error) { names, causes = append(names, name), append(causes, err) }

	const absent = "u0123456789abcdef01234567" // an instance nothing is installed for
	if _, err := resolveWindowsServiceInstance("PaperboatLocalDaemon", []string{"--instance", absent}); err == nil {
		t.Fatal("an absent installation was resolved")
	}
	if len(names) != 1 || names[0] != "PaperboatLocalDaemon-"+absent || causes[0] == nil {
		t.Fatalf("recorded %v %v", names, causes)
	}
	// Not an instance name: no installation, so no log an uninstall would have to find.
	for _, args := range [][]string{{"--instance", `..\..\Windows`}, {"--instance"}, nil} {
		if _, err := resolveWindowsServiceInstance("PaperboatLocalDaemon", args); err == nil {
			t.Fatalf("%v was resolved", args)
		}
	}
	if len(names) != 1 {
		t.Fatalf("a name that is not an instance was logged: %v", names)
	}
}
