//go:build darwin

package machineguard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNetworksetup answers like networksetup for one service and records every call.
type fakeNetworksetup struct{ calls []string }

func (f *fakeNetworksetup) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "-listallnetworkservices":
		return []byte("An asterisk (*) denotes that a network service is disabled.\nWi-Fi\n"), nil
	case "-getautoproxyurl":
		return []byte("URL: (null)\nEnabled: No\n"), nil
	}
	return nil, nil
}

func TestGuardSetsSystemAutoProxyOnceAndPurgeRestoresIt(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeNetworksetup{}
	manager, err := previewProxyManager(state, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(t.Context(), PreviewPACURL()); err != nil {
		t.Fatal(err)
	}
	want := "-setautoproxyurl Wi-Fi " + PreviewPACURL()
	if !contains(runner.calls, want) || !contains(runner.calls, "-setautoproxystate Wi-Fi on") {
		t.Fatalf("guard did not write the auto-proxy setting: %v", runner.calls)
	}
	if err := previewProxyLeftover(state); err == nil {
		t.Fatal("an unrestored proxy journal must be reported as a leftover")
	}
	if err := darwinRetiredPlatformState(state); err == nil {
		t.Fatal("the purge leftover inventory must include the proxy journal")
	}
	// The restore runs against the platform state the install left behind.
	restore := &restoringRunner{fakeNetworksetup: runner, url: PreviewPACURL()}
	if err := removePreviewProxyWith(t.Context(), state, restore); err != nil {
		t.Fatal(err)
	}
	if !contains(runner.calls, "-setautoproxystate Wi-Fi off") {
		t.Fatalf("previous (disabled) setting not restored: %v", runner.calls)
	}
	if _, err := os.Stat(filepath.Join(state, previewProxyJournal)); !os.IsNotExist(err) {
		t.Fatalf("journal left after restore: %v", err)
	}
	if err := darwinRetiredPlatformState(state); err != nil {
		t.Fatalf("leftovers after restore: %v", err)
	}
}

// restoringRunner reports the guard's PAC as installed until the state is switched off.
type restoringRunner struct {
	*fakeNetworksetup
	url string
	off bool
}

func (r *restoringRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	switch args[0] {
	case "-getautoproxyurl":
		r.calls = append(r.calls, strings.Join(args, " "))
		if r.off {
			return []byte("URL: (null)\nEnabled: No\n"), nil
		}
		return []byte("URL: " + r.url + "\nEnabled: Yes\n"), nil
	case "-setautoproxystate":
		r.off = args[2] == "off"
	}
	return r.fakeNetworksetup.Run(ctx, name, args...)
}

func contains(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func TestPurgeRestoresSystemProxyBeforeRemovingState(t *testing.T) {
	f := newDarwinPurgeFixture(t)
	if err := os.MkdirAll(f.paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.purge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.called("restore-proxy") {
		t.Fatalf("purge did not restore the system proxy; calls=%v", f.calls)
	}
}
