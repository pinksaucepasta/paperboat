// Package ownedpaths is the one list of the names Paperboat creates in a user's Paperboat directories (configuration,
// credentials, state, cache, logs, runtime). `pb uninstall` and `pb reset` remove exactly these and leave everything
// else, so a new file or directory in one of those places must be added here in the same commit.
package ownedpaths

import (
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Tree maps the name patterns (path.Match, lower case) of a directory's owned entries to what they hold. A nil Tree is
// a file, or a directory whose names are generated at run time and which Paperboat alone fills: all of it is owned.
type Tree map[string]Tree

// hex32 is a generated name of 16 bytes in hex.
var hex32 = strings.Repeat("[0-9a-f]", 32)

var diagnostics = Tree{
	"writer.lock": nil, "events-*": nil, "sentry-flood.json": nil, ".sentry-flood-*": nil,
	"last-unexpected-failure.txt": nil, "last-daemon-failure.txt": nil, "last-diagnostics-failure.txt": nil, ".last-diagnostics-failure.txt-*": nil,
}

// profile is auth.profile_dir (default <config>/credentials).
var profile = Tree{
	// <issuer key>.json, its journal and its locks
	"profiles":                    {hex32 + ".json": nil, hex32 + ".json.*": nil},
	"transactions":                {hex32 + ".json": nil},
	"pending-revocations":         {hex32 + ".json": nil, hex32 + "-" + hex32 + ".json": nil},
	"secrets":                     {"*.secret": nil},
	"environment-vaults":          {"*.sealed": nil},
	hex32 + hex32 + ".dpapi":      nil, // Windows secret store
	"peer-network-config-v1-*":    nil,
	"peer-network-relays-v1-*":    nil,
	"peer-local-trust-v1-*":       nil,
	"environment-host-key-*":      nil,
	"network-fingerprint-secret*": nil,
}

// hostRuntime is the host runtime's state root (--state-root).
var hostRuntime = Tree{
	"machine-identity.json": nil, "runtime-identity.json": nil, "machine-registration.json": nil,
	"machine-control.json": nil, ".machine-control-*": nil, "network-fingerprint-secret.json": nil,
	".network-fingerprint-secret-*": nil, "peer-endpoint.json": nil, ".peer-endpoint-*": nil,
	"renewal-refused.json": nil, ".runtime-identity-renewal.lock": nil,
	"bandwidth-usage.json": nil, "*-bandwidth-usage.json": nil, "*-offline-audit.json": nil, "*-offline-audit.json.adopt": nil, "bootstrap-resume.json": nil, "enrollment-token": nil,
	"state.db": nil, "state.db-wal": nil, "state.db-shm": nil, "state.db-journal": nil,
	"hostd-startup-error.log": nil, "carrier-startup-error.log": nil, "availability-policy.json": nil,
	"authorization": {"jwks.json": nil, "*-jwks.json": nil},
	"runtime":       nil, "recordings": nil, "file-transfers": nil, "preview": nil, "private-access": nil,
	"origin-credentials": nil, "config-sync": nil, "environment": nil, "peer-network": nil, "agent": nil,
	"updates": nil, "session-owners": nil, "workflows": {"tunnel-create": nil}, "hostd": {"fence.json": nil},
}

// app is <config|state|cache|data|logs|run>/paperboat. Several of these are one directory on macOS and Windows.
var app = Tree{
	"config.json": nil, "config.json.local-access.lock.d": nil, "*.json.workspaces": nil,
	"*.json.confirmation.json.*": nil, "*.preferences.json": nil,
	"credentials-location.json": nil, "telemetry.jsonl": nil, "telemetry.jsonl.[0-9]*": nil,
	"config-sync.jsonc": nil, "env-hook.json": nil, "recipe-key.json": nil, "recipe-apply.json": nil,
	"replaces-machine.json": nil, "dns.json": nil,
	"credentials":     profile,
	"machineguard":    {"trusted-ca": nil},
	"file-index.json": nil,
	"update-policy":   {hex32 + hex32 + ".json": nil},
	"daemon.lock":     nil, "daemon.lock.owner.json": nil, "install.lock": nil, "machine-inventory.json": nil,
	"daemon.sock": nil, "local-api.sock": nil, "paperboat-ssh-agent.sock": nil, "ssh_known_hosts": nil,
	"run":         {"local-api.sock": nil, "paperboat-ssh-agent.sock": nil, "ssh_known_hosts": nil, "daemon.sock": nil},
	"diagnostics": diagnostics,
	"bugreports":  {"bugreport-*.zip": nil},
	"runtime":     hostRuntime,
	"elevation":   {"request-*": nil},

	"paperboatd.log": nil, "paperboatd.log.1": nil,
}

func init() {
	// macOS and Windows keep the local daemon's state in <app>/state; Linux keeps it in <app> itself.
	app["state"] = app
}

// ForRoot is what Paperboat owns in the removal root at directory.
func ForRoot(directory string) Tree {
	name := strings.ToLower(filepath.Base(directory))
	switch name {
	case "paperboat", "state", "run":
		if name == "paperboat" {
			return app
		}
		return union(app, hostRuntime, profile) // also a --state-root or profile directory of that name
	case "credentials":
		return union(profile, hostRuntime)
	}
	if child, ok := app.Child(name); ok && child != nil && name != "runtime" {
		return child
	}
	return union(hostRuntime, profile)
}

func union(trees ...Tree) Tree {
	result := Tree{}
	for _, tree := range trees {
		for pattern, child := range tree {
			result[pattern] = child
		}
	}
	return result
}

// The names every writer derives from its file: locks, quarantined copies and the temporary files of atomic writes
// (renameio: "."+name+digits).
var derived = regexp.MustCompile(`(\.lock\.d\.stale-[0-9]+-[0-9]+|\.lock\.d|\.lock|\.corrupt|\.tmp|\.new)$`)
var temporary = regexp.MustCompile(`^\.(.+?)[0-9]+$`)

// windowsTemporary is atomicfile's temporary file on Windows.
var windowsTemporary = regexp.MustCompile(`^\.paperboat-[0-9a-f]{32}$`)

// Child reports whether name is owned in a directory holding tree, and what it holds in turn.
func (tree Tree) Child(name string) (Tree, bool) {
	if tree == nil {
		return nil, true
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		name = strings.ToLower(name)
	}
	if child, ok := tree[name]; ok {
		return child, true
	}
	for pattern, child := range tree {
		if ok, _ := path.Match(pattern, name); ok {
			return child, true
		}
	}
	if windowsTemporary.MatchString(name) {
		return nil, true
	}
	if base := derived.ReplaceAllString(name, ""); base != name {
		_, ok := tree.Child(base)
		return nil, ok
	}
	if match := temporary.FindStringSubmatch(name); match != nil {
		_, ok := tree.Child(match[1])
		return nil, ok
	}
	return nil, false
}
