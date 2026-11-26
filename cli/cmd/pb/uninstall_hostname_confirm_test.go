package main

import "testing"

func TestUninstallHostnameConfirmationIgnoresCaseOnlyOnWindows(t *testing.T) {
	for _, c := range []struct {
		goos, typed string
		want        bool
	}{
		{"windows", "VICTUS", true}, {"windows", "Victus", true}, {"windows", "victus2", false},
		{"linux", "VICTUS", false}, {"darwin", "VICTUS", false}, {"linux", "Victus", true},
	} {
		if got := hostnameConfirmed(c.goos, c.typed, "Victus"); got != c.want {
			t.Errorf("%s %q: got %v", c.goos, c.typed, got)
		}
	}
}
