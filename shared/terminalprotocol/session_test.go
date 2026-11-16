package terminalprotocol

import "testing"

func TestExitStatus(t *testing.T) {
	for _, test := range []struct {
		code   int
		signal string
		want   string
	}{
		{0, "", "exited 0"},
		{2, "", "exited 2"},
		{0, "SIGTERM", "exited 143 (SIGTERM)"},
		{0, "SIGKILL", "exited 137 (SIGKILL)"},
		{0, "SIGUSR1", "exited 1 (SIGUSR1)"},
		{130, "SIGINT", "exited 130 (SIGINT)"},
	} {
		if got := ExitStatus(test.code, test.signal); got != test.want {
			t.Errorf("ExitStatus(%d, %q) = %q, want %q", test.code, test.signal, got, test.want)
		}
	}
}
