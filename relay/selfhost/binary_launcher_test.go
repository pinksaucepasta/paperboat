package selfhost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBinaryLauncherRunsEdgeForTunnelCapability(t *testing.T) {
	binaries := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binaries, "paperboat-edge"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	launcher := binaryLauncher{directory: binaries}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := launcher.Validate(ctx, "tunnel", dir); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := launcher.Run(ctx, "tunnel", dir, &output); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "run\n--state-dir\n"+dir+"\n"; got != want {
		t.Fatalf("runtime arguments = %q, want %q", got, want)
	}
}
