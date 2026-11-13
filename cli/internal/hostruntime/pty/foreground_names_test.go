package pty

import "testing"

func TestHandlesInterruptItself(t *testing.T) {
	for image, want := range map[string]bool{
		"cmd.exe": true, "CMD.EXE": true, "powershell.exe": true, "PowerShell.exe": true, "pwsh.exe": true,
		"ping.exe": false, "python.exe": false, "": false, "powershell": false,
	} {
		if got := handlesInterruptItself(image); got != want {
			t.Errorf("handlesInterruptItself(%q) = %v, want %v", image, got, want)
		}
	}
}
