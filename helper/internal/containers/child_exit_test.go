package containers

import (
	"bytes"
	"errors"
	"fmt"
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

// A runtime that fails to start prints its reason, then shutdown records; the reason is what the
// journal must carry (oracle, 2026-11-05: address_in_use was hidden behind reporting_shutdown).
func TestLastOutputLinePrefersTheRuntimeFailureLineOverShutdownRecords(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho '{\"level\":\"ERROR\"}'\necho 'service failure stage=configure cause=address_in_use error_type=Errno'\necho 'paperboat-edge stopped; support reference x'\necho '{\"msg\":\"reporting_shutdown\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got := lastOutputLine("paperboat-pbh-test-edge"); got != "service failure stage=configure cause=address_in_use error_type=Errno" {
		t.Fatalf("explaining line = %q", got)
	}
}

func TestWriteLogTailWritesOnlyTheLastLinesDockerReturns(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1 $2 $3 $4 $5\" = 'logs --timestamps --tail 50 next' ] || exit 9\nfor i in $(seq 1 3); do echo \"line $i\"; done\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var w bytes.Buffer
	writeLogTail(&w, "next")
	for i := 1; i <= 3; i++ {
		if !strings.Contains(w.String(), fmt.Sprintf("pbh: handover replacement output: line %d\n", i)) {
			t.Fatalf("journal output = %q", w.String())
		}
	}
	w.Reset()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	writeLogTail(&w, "next")
	if w.Len() != 0 {
		t.Fatalf("a failed log read wrote %q", w.String())
	}
}
