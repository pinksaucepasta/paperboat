package daemoncmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDaemonOwnsRuntimeEntries(t *testing.T) {
	root := NewCommand()
	for _, name := range []string{"__runtime-host", "__runtime-hostd", "__runtime-worker", "__runtime-updated", "__runtime-activate", "__runtime-local-daemon", "__runtime-host-service", "__runtime-config", "__windows-sshd-service"} {
		command, _, err := root.Find([]string{name})
		if err != nil || command == root || command.Name() != name {
			t.Fatalf("missing daemon entry %s: %v", name, err)
		}
	}
	var output bytes.Buffer
	root.SetArgs([]string{"--help"})
	root.SetOut(&output)
	if err := root.ExecuteContext(context.Background()); err != nil || !strings.Contains(output.String(), "daemon") {
		t.Fatalf("help: err=%v output=%s", err, output.String())
	}
}

func TestDaemonRejectsInvalidInvocationBeforeStartup(t *testing.T) {
	for _, args := range [][]string{{"__local-daemon"}, {"__runtime-host", "extra"}, {"run", "extra"}, {"--unknown"}} {
		root := NewCommand()
		root.SetArgs(args)
		if err := root.ExecuteContext(context.Background()); err == nil {
			t.Fatalf("accepted invalid invocation %v", args)
		}
	}
}
