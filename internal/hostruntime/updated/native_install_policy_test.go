package updated

import "testing"

func TestWindowsNativeInstallRetiresOnlyCompletedTransactions(t *testing.T) {
	j := testWindowsActivationJournal()
	if nativeWindowsJournalRetirable(j) {
		t.Fatal("staged update retired")
	}
	j.Stage = windowsActivationRolledBack
	if !nativeWindowsJournalRetirable(j) {
		t.Fatal("completed rollback rejected")
	}
	j.Stage = windowsActivationCommitted
	if !nativeWindowsJournalRetirable(j) {
		t.Fatal("completed update rejected")
	}
	j.Stage = windowsActivationSwitching
	if nativeWindowsJournalRetirable(j) {
		t.Fatal("activating update retired")
	}
}
