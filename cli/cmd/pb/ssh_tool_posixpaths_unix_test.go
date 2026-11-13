//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shlex "github.com/anmitsu/go-shlex"
)

func TestRsyncRemoteShellUsesOwnRuntime(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	runtime := t.TempDir()
	for name, source := range map[string]string{"bash": bash, "cat": cat} {
		if err := os.Symlink(source, filepath.Join(runtime, name)); err != nil {
			t.Fatal(err)
		}
	}
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "cat"), []byte("#!/bin/sh\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "binary round trip", true: "ssh exit status"}[fail], func(t *testing.T) {
			shell := cat
			if fail {
				shell = filepath.Join(t.TempDir(), "ssh")
				if err := os.WriteFile(shell, []byte("#!/bin/sh\nexit 17\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			args, err := shlex.Split(strings.TrimPrefix(rsyncRemoteShellArgument(filepath.Join(runtime, "bash"), shell), "--rsh="), true)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, args[0], args[1:]...)
			command.Env = withEnvironment(os.Environ(), "PATH", foreign)
			input := bytes.Repeat([]byte{0, 1, 10, 13, 128, 255}, 1024)
			command.Stdin = bytes.NewReader(input)
			command.WaitDelay = time.Second
			var output, stderr bytes.Buffer
			command.Stdout, command.Stderr = &output, &stderr
			err = command.Run()
			if fail {
				if command.ProcessState == nil || command.ProcessState.ExitCode() != 17 {
					t.Fatalf("exit = %v; stderr = %s", err, &stderr)
				}
			} else if err != nil || !bytes.Equal(output.Bytes(), input) {
				t.Fatalf("round trip: %v, got %d bytes, want %d; stderr = %s", err, output.Len(), len(input), &stderr)
			}
		})
	}
}
