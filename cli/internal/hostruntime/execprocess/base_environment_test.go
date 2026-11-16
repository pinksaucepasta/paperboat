package execprocess

import "testing"

// Windows owner environment blocks hold names such as ProgramFiles(x86); the base environment is ours, not client input.
func TestValidBaseEnvironmentAcceptsWindowsNames(t *testing.T) {
	if !validBaseEnvironment([]string{"ProgramFiles(x86)=C:\\Program Files (x86)", "Path=C:\\Windows", "OS=Windows_NT"}) {
		t.Fatal("owner environment names rejected")
	}
	for _, bad := range [][]string{{"=value"}, {"NOEQUALS"}, {"A\nB=1"}, {"Path=1", "Path=2"}} {
		if validBaseEnvironment(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}
