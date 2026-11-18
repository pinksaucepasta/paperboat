//go:build darwin || linux

package service

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"howett.net/plist"
)

func TestUnixUpdateActivatorUsesFixedPersistentRecoveryEntry(t *testing.T) {
	environment := map[string]string{"PAPERBOAT_UPDATE_STATE_ROOT": "/var/lib/paperboat/updated", "PAPERBOAT_BINARY": "/opt/paperboat/bin/pb", "PAPERBOAT_ENROLLED_UID": "1001"}
	binary := "/var/lib/paperboat/updated/activation/pb"
	body, err := unixUpdateActivatorDefinition("linux", 501, binary, environment)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"` + binary + `" "daemon" "__runtime-updated" "--activation-helper"`, "Restart=on-failure\nRestartSec=5s\nRestartSteps=3\nRestartMaxDelaySec=30s\n", "WantedBy=multi-user.target", "Nice=10\n", "IOSchedulingClass=idle\n"} {
		if !strings.Contains(string(body), expected) {
			t.Fatalf("missing %q in %s", expected, body)
		}
	}
	if strings.Contains(string(body), "ExecStop") || strings.Contains(string(body), "--user") {
		t.Fatalf("helper must not own hostd or user daemon lifecycle: %s", body)
	}
	body, err = unixUpdateActivatorDefinition("darwin", 501, binary, environment)
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		Label, UserName  string
		ProgramArguments []string
		RunAtLoad        bool
		KeepAlive        map[string]bool
	}
	if _, err = plist.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	if job.Label != UnixUpdateActivatorLabel+".u501" || job.UserName != "root" || !job.RunAtLoad || len(job.ProgramArguments) != 4 || job.ProgramArguments[3] != "--activation-helper" || job.KeepAlive["SuccessfulExit"] {
		t.Fatalf("unsafe helper job: %#v", job)
	}
}
func TestUnixUpdateActivatorRejectsInheritedExecutableOverrides(t *testing.T) {
	for _, key := range []string{"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "PATH", "HOME", "NOTIFY_SOCKET"} {
		if _, err := unixUpdateActivatorDefinition("linux", 501, "/safe/activation/pb", map[string]string{key: "/untrusted"}); err == nil {
			t.Fatalf("accepted %s", key)
		}
	}
	if _, err := unixUpdateActivatorDefinition("linux", 501, "/safe/activation/pb", map[string]string{"PAPERBOAT_BINARY": "/safe/pb\nExecStart=/bad"}); err == nil {
		t.Fatal("accepted newline injection")
	}
}

// The ordinary updater and its persistent helper need the same installer-owned
// source identity to authorize recovery of a locally installed runtime.
func TestUnixUpdateActivatorPreservesInstallerSource(t *testing.T) {
	const source = "eyJkaXN0cmlidXRpb24iOiJvZmZpY2lhbCJ9"
	environment := map[string]string{"PAPERBOAT_INSTALL_SOURCE": source}
	for _, platform := range []string{"linux", "darwin"} {
		body, err := unixUpdateActivatorDefinition(platform, 501, "/safe/activation/pb", environment)
		if err != nil {
			t.Fatalf("%s: %v", platform, err)
		}
		if !strings.Contains(string(body), source) {
			t.Fatalf("%s: installer source omitted", platform)
		}
	}
	environment["PAPERBOAT_INSTALL_SOURCE"] = source + "\nExecStart=/bad"
	if _, err := unixUpdateActivatorDefinition("linux", 501, "/safe/activation/pb", environment); err == nil {
		t.Fatal("accepted injected source")
	}
}

// A crash and reboot between the helper's Install and Retire, across a release that changed the unit text, left
// our own helper unit behind; the next Install refused it as "another installation" and the updater could not
// start. A unit that runs the same command is ours and is replaced; any other command is not.
func TestUnixUpdateActivatorRecognisesItsOwnStaleUnit(t *testing.T) {
	environment := map[string]string{"PAPERBOAT_UPDATE_STATE_ROOT": "/var/lib/paperboat/updated", "PAPERBOAT_ENROLLED_UID": "1001"}
	binary := "/var/lib/paperboat/updated/activation/pb"
	for _, platform := range []string{"linux", "darwin"} {
		wanted, err := unixUpdateActivatorDefinition(platform, 501, binary, environment)
		if err != nil {
			t.Fatal(err)
		}
		older := wanted
		if platform == "linux" {
			older = []byte(strings.ReplaceAll(string(wanted), "RestartSteps=3\nRestartMaxDelaySec=30s\n", "RestartRandomizedDelaySec=1s\n"))
			if string(older) == string(wanted) {
				t.Fatal("the stale text equals the new one")
			}
		}
		if !sameActivatorJob(platform, older, wanted) {
			t.Fatalf("%s: our own stale unit was not recognised:\n%s", platform, older)
		}
		other, err := unixUpdateActivatorDefinition(platform, 501, "/var/lib/paperboat/updated/other/pb", environment)
		if err != nil {
			t.Fatal(err)
		}
		for name, current := range map[string][]byte{"another command": other, "garbage": []byte("not a unit"), "empty": nil} {
			if sameActivatorJob(platform, current, wanted) {
				t.Fatalf("%s: %s was taken for our unit", platform, name)
			}
		}
	}
	// A unit with two commands is not one job of ours.
	wanted, _ := unixUpdateActivatorDefinition("linux", 501, binary, environment)
	if sameActivatorJob("linux", append(append([]byte(nil), wanted...), []byte("ExecStart=/bin/true\n")...), wanted) {
		t.Fatal("a unit with a second ExecStart was taken for ours")
	}
}

type recordingRunner struct{ commands []string }

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) error {
	r.commands = append(r.commands, name+" "+strings.Join(args, " "))
	return nil
}

// Install replaces the stale unit of ours on disk, and still refuses one that runs another command.
func TestUnixUpdateActivatorInstallReplacesItsOwnStaleUnit(t *testing.T) {
	if os.Geteuid() != 0 || runtime.GOOS != "linux" {
		t.Skip("requires root on linux: the unit lives in /etc/systemd/system")
	}
	root, err := os.MkdirTemp("/var/lib", "pb-activator-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err = os.MkdirAll(filepath.Join(root, "activation"), 0755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "activation", "pb")
	if err = os.WriteFile(binary, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	activator := UnixUpdateActivator{Runner: runner, Platform: "linux", UID: 64998}
	defer os.Remove(activator.definition())
	environment := map[string]string{"PAPERBOAT_UPDATE_STATE_ROOT": root}
	wanted, err := unixUpdateActivatorDefinition("linux", 64998, binary, environment)
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.ReplaceAll(string(wanted), "RestartSteps=3\nRestartMaxDelaySec=30s\n", "RestartRandomizedDelaySec=1s\n")
	if err = os.WriteFile(activator.definition(), []byte(stale), 0644); err != nil {
		t.Fatal(err)
	}
	if err = activator.Install(context.Background(), binary, environment); err != nil {
		t.Fatalf("our own stale helper unit was refused: %v", err)
	}
	if got, _ := os.ReadFile(activator.definition()); string(got) != string(wanted) {
		t.Fatalf("the stale unit was not replaced:\n%s", got)
	}
	foreign := strings.ReplaceAll(string(wanted), binary, "/usr/bin/other")
	if err = os.WriteFile(activator.definition(), []byte(foreign), 0644); err != nil {
		t.Fatal(err)
	}
	if err = activator.Install(context.Background(), binary, environment); err == nil {
		t.Fatal("a unit that runs another command was replaced")
	}
}
