//go:build windows

package windowsopenssh

import (
	"strings"
	"testing"
)

func TestOwnedServiceCommandRequiresExactPaperboatExecutable(t *testing.T) {
	config := DefaultConfig(nil)
	expected := `C:\Program Files\Paperboat\bin\pb.exe`
	command := `"` + expected + `" daemon __windows-sshd-service --sshd "C:\Program Files\OpenSSH\sshd.exe" --config C:\ProgramData\Paperboat\ssh\sshd_config`
	if !sameOwnedServiceCommand(command, expected, config) {
		t.Fatal("exact Paperboat service command was rejected")
	}
	for _, invalid := range []string{
		strings.Replace(command, " daemon ", " ", 1),
		strings.Replace(command, " daemon ", " run ", 1),
		command + " --unexpected",
	} {
		if sameOwnedServiceCommand(invalid, expected, config) {
			t.Fatal("invalid daemon invocation was accepted as Paperboat-owned")
		}
	}
	foreign := `"C:\Program Files\Foreign\service.exe" daemon __windows-sshd-service --sshd "C:\Program Files\OpenSSH\sshd.exe" --config C:\ProgramData\Paperboat\ssh\sshd_config`
	if sameOwnedServiceCommand(foreign, expected, config) {
		t.Fatal("foreign service executable was accepted as Paperboat-owned")
	}
}
