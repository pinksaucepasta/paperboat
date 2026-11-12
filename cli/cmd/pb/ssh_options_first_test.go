package main

import "testing"

// `pb ssh m -- -N -L ...` on a Windows client goes to ssh.exe; `pb ssh m -- hostname` stays a remote command.
func TestOpenSSHOptionsFirst(t *testing.T) {
	for name, test := range map[string]struct {
		args []string
		want bool
	}{
		"no arguments":         {nil, false},
		"remote command":       {[]string{"hostname"}, false},
		"command with options": {[]string{"echo", "-n", "x"}, false},
		"forward":              {[]string{"-N", "-L", "1:127.0.0.1:2"}, true},
		"option value":         {[]string{"-o", "ExitOnForwardFailure=yes", "host"}, true},
		"lone dash":            {[]string{"-"}, false},
	} {
		if got := opensshOptionsFirst(test.args); got != test.want {
			t.Errorf("%s: opensshOptionsFirst(%q) = %v, want %v", name, test.args, got, test.want)
		}
	}
}
