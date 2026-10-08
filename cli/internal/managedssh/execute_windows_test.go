//go:build windows

package managedssh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenSSHExecutorWindowsNativeLifecycle(t *testing.T) {
	switch os.Getenv("PB_NATIVE_EXECUTOR_HELPER") {
	case "exit":
		os.Exit(7)
	case "wait":
		if err := os.WriteFile(os.Getenv("PB_NATIVE_EXECUTOR_READY"), nil, 0600); err != nil {
			os.Exit(8)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-test.run=^TestOpenSSHExecutorWindowsNativeLifecycle$"}
	err = (OpenSSHExecutor{}).Execute(t.Context(), binary, args, append(os.Environ(), "PB_NATIVE_EXECUTOR_HELPER=exit"))
	var status NativeExitError
	var original *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 7 || !errors.As(err, &original) {
		t.Fatal("native Windows status/cause lost")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ready := filepath.Join(t.TempDir(), "ready")
	done := make(chan error, 1)
	go func() {
		done <- (OpenSSHExecutor{}).Execute(ctx, binary, args, append(os.Environ(), "PB_NATIVE_EXECUTOR_HELPER=wait", "PB_NATIVE_EXECUTOR_READY="+ready))
	}()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("native child exited before readiness: %v", err)
		case <-ctx.Done():
			t.Fatal("native child did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.As(err, &status) || status.ExitCode() != 1 || !errors.As(err, &original) {
			t.Fatal("job cancellation status/cause lost")
		}
	case <-time.After(time.Second):
		t.Fatal("native job cancellation did not reap child")
	}
}

func TestValidEnvironmentAcceptsWindowsDriveCurrentDirectory(t *testing.T) {
	for _, values := range [][]string{
		nil,
		{"Path=C:\\Windows\\System32"},
		{"=C:=C:\\Users\\Pujan", "Path=C:\\Windows\\System32"},
		{"=z:=Z:/workspace"},
	} {
		if !validEnvironment(values) {
			t.Fatalf("validEnvironment(%q) = false", values)
		}
	}
}

func TestValidEnvironmentRejectsMalformedWindowsEntries(t *testing.T) {
	for _, value := range []string{
		"missing-separator",
		"=C=C:\\Users\\Pujan",
		"=CC:=C:\\Users\\Pujan",
		"=1:=1:\\Users\\Pujan",
		"=C:=D:\\Users\\Pujan",
		"=C:=C:relative",
		"=C:=relative",
		"=C:=",
		"==C:\\Users\\Pujan",
		"NAME=contains\x00nul",
		strings.Repeat("x", 1<<20+1) + "=value",
	} {
		if validEnvironment([]string{value}) {
			t.Fatalf("validEnvironment(%q) = true", value)
		}
	}
}
