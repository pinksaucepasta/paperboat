package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// Keep local test servers independent of the developer's system proxy settings.
	if err := os.Setenv("NO_PROXY", "127.0.0.1,localhost,::1"); err != nil {
		panic(err)
	}
	// A unit test never talks to the host's real pb daemon: on a machine with a live installed daemon, TestENVNotSetUpIsNamedNotAuthentication
	// read its snapshot (control_plane_unavailable) and named the wrong problem. The daemon's state and socket
	// directories are private to the test process; tests that need a daemon say where it is in their own context.
	private, err := os.MkdirTemp("", "pb-test-")
	if err != nil {
		panic(err)
	}
	for name, directory := range map[string]string{"XDG_STATE_HOME": "state", "XDG_RUNTIME_DIR": "runtime"} {
		if err := os.Mkdir(filepath.Join(private, directory), 0o700); err != nil {
			panic(err)
		}
		if err := os.Setenv(name, filepath.Join(private, directory)); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(private)
	os.Exit(code)
}
