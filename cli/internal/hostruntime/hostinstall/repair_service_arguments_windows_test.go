//go:build windows

package hostinstall

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// The repair reconcile must accept the command lines the install plan registers (hostd carries its generation) and
// nothing else; pb doctor --repair failed with ErrInvalidRequest on every healthy install while it demanded five tokens.
func TestWindowsRuntimeServiceArgumentsMatchPlan(t *testing.T) {
	const instance, generation = "u0123456789abcdef01234567", "g1"
	roles := map[string]string{service.HostdKind: "__runtime-hostd", service.DaemonKind: "__runtime-local-daemon", service.UpdaterKind: "__runtime-updated"}
	for _, definition := range windowsRuntimeServiceDefinitions(service.Layout{Instance: instance}, generation) {
		line := append([]string{`C:\pb.exe`}, definition.arguments...)
		role := roles[definition.kind]
		if !windowsRuntimeServiceArguments(line, role, instance, generation) {
			t.Errorf("%s: planned command line %v rejected", definition.kind, line)
		}
		if windowsRuntimeServiceArguments(line, role, instance, "other") && role == "__runtime-hostd" {
			t.Errorf("%s: another generation accepted", definition.kind)
		}
		if windowsRuntimeServiceArguments(line[:len(line)-1], role, instance, generation) {
			t.Errorf("%s: truncated command line accepted", definition.kind)
		}
	}
}
