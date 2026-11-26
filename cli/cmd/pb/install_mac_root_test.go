package main

import (
	"errors"
	"strings"
	"testing"
)

func TestInstallRefusesRootOnMacOSUpFront(t *testing.T) {
	var refusal commandRejection
	if err := refuseMacRootInstall("darwin", 0); !errors.As(err, &refusal) || !refusal.valid() || !strings.Contains(err.Error(), "normal macOS user") {
		t.Fatalf("root on macOS: got %v, want the up-front refusal", err)
	}
	for _, c := range []struct {
		platform string
		euid     int
	}{{"darwin", 501}, {"linux", 0}, {"linux", 1000}} {
		if err := refuseMacRootInstall(c.platform, c.euid); err != nil {
			t.Fatalf("%s euid %d: got %v, want no refusal", c.platform, c.euid, err)
		}
	}
}
