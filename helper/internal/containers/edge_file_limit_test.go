package containers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The edge container gets the file limit its listeners need; the relay container keeps docker's default.
func TestEdgeContainerRaisesFileLimitRelayDoesNot(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "args")
	script := "#!/bin/sh\necho \"$@\" >> \"$FIXTURE_LOG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXTURE_LOG", log)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	l := &Launcher{StateDir: t.TempDir()}
	for _, capability := range []string{"relay", "tunnel"} {
		if err := l.startContainerNamed(t.Context(), capability, "image", "paperboat-pbh-test-"+capability); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(runs)), "\n")
	if len(lines) != 2 {
		t.Fatalf("docker run calls: %q", lines)
	}
	if strings.Contains(lines[0], "nofile") || !strings.Contains(lines[1], "--ulimit nofile=1048576:1048576") {
		t.Fatalf("relay args %q, edge args %q", lines[0], lines[1])
	}
}
