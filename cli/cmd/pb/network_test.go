package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
)

func TestMain(m *testing.M) {
	// Keep local test servers independent of the developer's system proxy settings.
	if err := os.Setenv("NO_PROXY", "127.0.0.1,localhost,::1"); err != nil {
		panic(err)
	}
	// A unit test never talks to the host's real pb daemon: on a machine with a live installed daemon, TestENVNotSetUpIsNamedNotAuthentication
	// read its snapshot (control_plane_unavailable) and named the wrong problem. Tests that need a snapshot set their own readDaemonSnapshot.
	readDaemonSnapshot = func(context.Context) (localapi.Snapshot, error) {
		return localapi.Snapshot{}, errors.New("unit tests do not read the host's daemon")
	}
	os.Exit(m.Run())
}
