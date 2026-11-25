package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Oracle 2026-11-22: a pb reset or uninstall removed the whole ~/.config/paperboat, and credentials the user had put
// there with it. Only what Paperboat creates is removed; the rest stays, is reported, and no link is followed.
func TestUserStateRemovalLeavesWhatPaperboatDidNotCreate(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside := filepath.Join(base, "config", "paperboat"), filepath.Join(base, "outside")
	h32 := strings.Repeat("ab", 16)
	owned := []string{
		"config.json", ".config.json123456", "config.json.local-access.lock.d/owner.json", "config.json.workspaces/x.json",
		"config.json.confirmation.json.ABC234", "config.preferences.json", "config.preferences.json.lock", "telemetry.jsonl", "telemetry.jsonl.4",
		"credentials-location.json", "config-sync.jsonc", "env-hook.json", "recipe-key.json", "recipe-apply.json", "daemon.lock.owner.json.new", "runtime/carrier-0-offline-audit.json", "runtime/hostd/fence.json", "replaces-machine.json", ".paperboat-" + h32,
		"credentials/profiles/" + h32 + ".json", "credentials/profiles/" + h32 + ".json.lock.d/owner.json", "credentials/profiles/" + h32 + ".json.refresh.json",
		"credentials/transactions/" + h32 + ".json", "credentials/pending-revocations/" + h32 + "-" + h32 + ".json", "credentials/secrets/profile-x.secret",
		"credentials/" + h32 + h32 + ".dpapi", "credentials/" + h32 + h32 + ".dpapi.lock", "credentials/network-fingerprint-secret.lock.d/owner.json",
		"credentials/environment-vaults/environment-password-vault-v1-x.sealed.lock.d.stale-1-2/owner.json",
		"file-index.json", "update-policy/" + h32 + h32 + ".json", "install.lock", "daemon.lock", "daemon.sock.lock", "diagnostics/events-1-00.ndjson",
		"bugreports/bugreport-s.zip", "elevation/request-x/helper.exe", "machineguard/trusted-ca/a.pem", "paperboatd.log.1",
		"runtime/state.db-wal", "runtime/carrier-0-bandwidth-usage.json", "runtime/.runtime-identity-renewal.lock", "runtime/authorization/carrier-0-jwks.json", "runtime/peer-network/secrets/x",
		"runtime/workflows/tunnel-create/" + h32 + h32 + ".json.lock", "state/runtime/workflows/tunnel-create/" + h32 + h32 + ".json",
		"state/machine-inventory.json", "state/run/local-api.sock",
	}
	foreign := []string{"operator.password", "config.json.bak", "notes/todo.txt", "credentials/aws.key", "credentials/profiles/mine.json", "runtime/mine.db", "state/mine.txt", "diagnostics/mine.log", "elevation/mine"}
	for _, name := range append(slices.Clone(owned), foreign...) {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("kept"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "state.db")
	if err := os.WriteFile(sentinel, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := true
	for _, link := range []string{"mine-link", "run", filepath.Join("runtime", "updates")} {
		if err := os.Symlink(outside, filepath.Join(root, link)); err != nil {
			if runtime.GOOS != "windows" {
				t.Fatal(err)
			}
			links = false
		}
	}
	want := []string{}
	for _, name := range foreign {
		want = append(want, filepath.Join(root, filepath.FromSlash(name)))
	}
	want[2] = filepath.Join(root, "notes")
	if links {
		want = append(want, filepath.Join(root, "mine-link"))
	}
	slices.Sort(want)
	if got := foreignUserPaths([]string{root, filepath.Join(root, "runtime")}, nil); !slices.Equal(got, want) {
		t.Fatalf("reported as left:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" { // one directory in two spellings is listed once
		other := filepath.Join(filepath.Dir(root), "Paperboat", "runtime")
		if got := foreignUserPaths([]string{root, other}, nil); len(got) != len(want) {
			t.Fatalf("reported twice: %v", got)
		}
	}
	// A reset keeps the pairing note and the diagnostics; what the user has beside them is reported all the same.
	kept := []string{filepath.Join(root, "replaces-machine.json"), filepath.Join(root, "diagnostics")}
	besides := slices.DeleteFunc(slices.Clone(want), func(path string) bool { return pathWithinOrEqual(kept[1], path) })
	if got := foreignUserPaths([]string{root}, kept); !slices.Equal(got, besides) {
		t.Fatalf("reset reported as left:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(besides, "\n"))
	}
	if err := removePathPreserving(root, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range owned {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Paperboat's %s survived: %v", name, err)
		}
	}
	for _, path := range append(want, sentinel) {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("foreign %s: %v", path, err)
		}
	}
	for _, gone := range []string{"run", "update-policy", filepath.Join("runtime", "updates"), filepath.Join("credentials", "secrets")} {
		if _, err := os.Lstat(filepath.Join(root, gone)); links && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived: %v", gone, err)
		}
	}
	// Nothing foreign left: the directory goes too.
	for _, path := range want {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := removePathPreserving(root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the empty directory survived: %v", err)
	}
	// A custom profile or state directory that is a folder of the user's keeps the user's files.
	custom := filepath.Join(base, "documents")
	for _, name := range []string{"thesis.txt", "state.db"} {
		if err := os.MkdirAll(custom, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(custom, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removePathPreserving(custom, nil); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(custom); err != nil || len(entries) != 1 || entries[0].Name() != "thesis.txt" {
		t.Fatalf("custom directory: %v %v", entries, err)
	}
	// The directory itself linked elsewhere is not entered.
	if links {
		linked := filepath.Join(base, "linked", "paperboat")
		if err := os.MkdirAll(filepath.Dir(linked), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, linked); err != nil {
			t.Fatal(err)
		}
		if err := removePathPreserving(linked, nil); err != nil {
			t.Fatal(err)
		}
		if got := foreignUserPaths([]string{linked}, nil); !slices.Equal(got, []string{linked}) {
			t.Fatalf("linked root reported as %v", got)
		}
		if _, err := os.Lstat(sentinel); err != nil {
			t.Fatal(err)
		}
	}
}
