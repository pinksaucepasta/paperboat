//go:build darwin || linux

package service

import (
	"context"
	"errors"
	"howett.net/plist"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestUnixUpdateActivatorUsesFixedPersistentRecoveryEntry(t *testing.T) {
	environment := map[string]string{"PAPERBOAT_UPDATE_STATE_ROOT": "/var/lib/paperboat/updated", "PAPERBOAT_BINARY": "/opt/paperboat/bin/pb", "PAPERBOAT_ENROLLED_UID": "1001"}
	binary := "/var/lib/paperboat/updated/activation/pb"
	body, err := unixUpdateActivatorDefinition("linux", binary, environment)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"` + binary + `" "daemon" "__runtime-updated" "--activation-helper"`, "Restart=on-failure", "WantedBy=multi-user.target"} {
		if !strings.Contains(string(body), expected) {
			t.Fatalf("missing %q in %s", expected, body)
		}
	}
	if strings.Contains(string(body), "ExecStop") || strings.Contains(string(body), "--user") {
		t.Fatalf("helper must not own hostd or user daemon lifecycle: %s", body)
	}
	body, err = unixUpdateActivatorDefinition("darwin", binary, environment)
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
	if job.Label != UnixUpdateActivatorLabel || job.UserName != "root" || !job.RunAtLoad || len(job.ProgramArguments) != 4 || job.ProgramArguments[3] != "--activation-helper" || job.KeepAlive["SuccessfulExit"] {
		t.Fatalf("unsafe helper job: %#v", job)
	}
}
func TestUnixUpdateActivatorRejectsInheritedExecutableOverrides(t *testing.T) {
	for _, key := range []string{"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "PATH", "HOME", "NOTIFY_SOCKET"} {
		if _, err := unixUpdateActivatorDefinition("linux", "/safe/activation/pb", map[string]string{key: "/untrusted"}); err == nil {
			t.Fatalf("accepted %s", key)
		}
	}
	if _, err := unixUpdateActivatorDefinition("linux", "/safe/activation/pb", map[string]string{"PAPERBOAT_BINARY": "/safe/pb\nExecStart=/bad"}); err == nil {
		t.Fatal("accepted newline injection")
	}
}

func TestUnixUpdateActivatorRestartsOnlyFixedHostdJob(t *testing.T) {
	runner := &commandRunner{}
	controller := UnixUpdateActivator{Platform: runtime.GOOS, UID: 501, Runner: runner}
	if err := controller.RestartHostd(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl", "restart", "paperboat-hostd.service"}
	if runtime.GOOS == "darwin" {
		want = []string{"launchctl", "kickstart", "-k", "system/" + HostdLabel}
	}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("calls=%v", runner.calls)
	}
	failed := errors.New("native restart failed")
	runner.errAt = 2
	runner.customError = failed
	if err := controller.RestartHostd(context.Background()); !errors.Is(err, failed) {
		t.Fatalf("restart failure=%v", err)
	}
}
