package containers

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake docker prints 10 KiB of noise and then one error line, so the test
// proves both the bound and the choice of the final line without Docker.
func TestLastOutputLineIsBoundedAndTakenFromContainerLogs(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nhead -c 10000 /dev/zero | tr '\\0' x\necho\necho 'error: configure failed at /state/relay/edge.json'\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got := lastOutputLine("paperboat-pbh-test-relay"); got != "error: configure failed at /state/relay/edge.json" {
		t.Fatalf("last output line = %q", got)
	}
}

func TestLineTailNeverKeepsMoreThanItsBound(t *testing.T) {
	tail := &lineTail{}
	if _, err := tail.Write([]byte(strings.Repeat("a", 10000) + "\nlast\n")); err != nil {
		t.Fatal(err)
	}
	if len(tail.buf) > childTailBytes {
		t.Fatalf("tail kept %d bytes, bound is %d", len(tail.buf), childTailBytes)
	}
	if got := tail.lastLine(); got != "last" {
		t.Fatalf("last line = %q", got)
	}
	if got := (&lineTail{}).lastLine(); got != "" {
		t.Fatalf("empty output produced a line: %q", got)
	}
}

func TestExitErrorAttachesLineOnlyForContainerExit(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho 'permission denied'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := exitError("paperboat-pbh-test-relay", errors.New("runtime container exited before readiness"))
	if err == nil {
		t.Fatal("exit error was dropped")
	}
	var exit *ChildExit
	if errors.As(err, &exit) {
		t.Fatalf("a non-exit error was marked as a container exit: %v", err)
	}
	err = exitError("paperboat-pbh-test-relay", errors.Join(errContainerExited, errors.New("before readiness")))
	if !errors.As(err, &exit) || exit.LastLine != "permission denied" || !errors.Is(err, errContainerExited) {
		t.Fatalf("container exit did not carry its last line: %#v", err)
	}
}
