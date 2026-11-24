package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
)

func TestStatusDoesNotClaimSSHReadyWhileServerIsStopped(t *testing.T) {
	stored := localapi.MachineStatus{ID: "m1", Alias: "box", SSHReadiness: "ready", Health: make([]localapi.HealthItem, 0, 4)}
	up, down := stored, stored
	refreshSSHReadiness(&up, func(string) error { return nil })
	refreshSSHReadiness(&down, func(string) error { return &managedssh.RefusedError{Reason: managedssh.RefusedServerUnreachable} })
	for _, test := range []struct {
		machine localapi.MachineStatus
		checked bool
		want    string
	}{
		{stored, false, "SSH: on\n"},
		{up, true, "SSH: ready\n"},
		{down, true, "SSH: degraded\n"},
		{down, true, "could not connect to its SSH server"},
	} {
		var out bytes.Buffer
		writeStatus(&out, localapi.Snapshot{Machines: []localapi.MachineStatus{test.machine}}, test.checked)
		if !strings.Contains(out.String(), test.want) {
			t.Errorf("want %q in:\n%s", test.want, out.String())
		}
	}
	offline := localapi.MachineStatus{SSHReadiness: "degraded"}
	refreshSSHReadiness(&offline, func(string) error { t.Fatal("asked a machine that is not ready"); return nil })
}
