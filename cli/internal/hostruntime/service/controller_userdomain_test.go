//go:build !windows

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// describeMac makes launchctl answer like a Mac whose user has (or has no) session at the screen, and
// where the agent's service is (or is not) already loaded in user/<uid>.
func describeMac(t *testing.T, guiPresent, loadedInUserDomain bool) {
	t.Helper()
	previous := launchctlExit
	t.Cleanup(func() { launchctlExit = previous })
	launchctlExit = func(args ...string) int {
		target := args[len(args)-1]
		switch {
		case strings.HasPrefix(target, "user/") && strings.Contains(target, "/") && strings.Count(target, "/") == 2:
			if loadedInUserDomain {
				return 0
			}
			return 113
		case strings.HasPrefix(target, "gui/") && !guiPresent:
			return launchdDomainUnsupported
		}
		return 0
	}
}

func TestPerUserAgentUsesTheUserDomainWhenTheUserHasNoSessionAtTheScreen(t *testing.T) {
	controller := LaunchdController{Runner: &outputCommandRunner{}, UID: 502, Label: DaemonLabel, UserDomain: true}
	for _, test := range []struct {
		name                       string
		guiPresent, loadedInUserDo bool
		want                       string
	}{
		{"logged in at the screen", true, false, "gui/502"},
		{"over SSH only", false, false, "user/502"},
		{"started over SSH, console login later", true, true, "user/502"},
	} {
		t.Run(test.name, func(t *testing.T) {
			describeMac(t, test.guiPresent, test.loadedInUserDo)
			if got := controller.domain(); got != test.want {
				t.Fatalf("domain=%s want %s", got, test.want)
			}
		})
	}
}

func TestPerUserAgentDeclaresTheBackgroundSessionOnlyInTheUserDomain(t *testing.T) {
	for _, test := range []struct {
		guiPresent bool
		want       bool
	}{{true, false}, {false, true}} {
		describeMac(t, test.guiPresent, false)
		body, err := renderLaunchd(Config{Platform: "darwin", Kind: DaemonKind, Executable: "/Users/t/.local/bin/pb", User: "t", Group: "staff",
			Controller: LaunchdController{Runner: &outputCommandRunner{}, UID: 502, Label: DaemonLabel, UserDomain: true}})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(body), "LimitLoadToSessionType") && strings.Contains(string(body), "<string>Background</string>"); got != test.want {
			t.Fatalf("gui=%v: LimitLoadToSessionType present=%v want %v\n%s", test.guiPresent, got, test.want, body)
		}
	}
}

// pb uninstall as a user with no session at the screen failed at "stop local daemon" (launchctl bootout
// gui/<uid>: 125 Domain does not support specified action) and kept the rest of the removal waiting.
func TestRemovingAPerUserAgentReachesEveryDomainAndToleratesAMissingGUIDomain(t *testing.T) {
	describeMac(t, false, true)
	unsupported := errors.New("launchctl bootout: Boot-out failed: 125: Domain does not support specified action")
	absent := errors.New("launchctl print: Could not find service in domain: No such process")
	runner := &outputCommandRunner{errors: []error{unsupported, nil, absent}}
	controller := LaunchdController{Runner: runner, UID: 502, Label: DaemonLabel, UserDomain: true}
	if err := controller.Remove(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"launchctl bootout gui/502/" + DaemonLabel,
		"launchctl bootout user/502/" + DaemonLabel,
		"launchctl print user/502/" + DaemonLabel,
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("calls=%v", runner.calls)
	}
	for index, call := range runner.calls {
		if got := strings.Join(call, " "); got != want[index] {
			t.Fatalf("call %d=%q want %q", index, got, want[index])
		}
	}
}

// An agent loaded in the user domain while the console session's gui domain exists is booted out of gui before the
// declaration is applied again, so one job runs and start/restart reach it.
func TestApplyingAPerUserAgentBootsItOutOfTheOtherDomain(t *testing.T) {
	describeMac(t, true, true) // chosen domain: user/502 (it already holds the job)
	absent := errors.New("launchctl bootout: Could not find service in domain: No such process")
	runner := &outputCommandRunner{errors: []error{absent}}
	controller := LaunchdController{Runner: runner, UID: 502, Label: DaemonLabel, UserDomain: true}
	_ = controller.Apply(context.Background(), "/x/agent.plist", false)
	if len(runner.calls) < 1 || strings.Join(runner.calls[0], " ") != "launchctl bootout gui/502/"+DaemonLabel {
		t.Fatalf("calls=%v, want the first call to boot the job out of gui/502", runner.calls)
	}
	for _, call := range runner.calls {
		if strings.Join(call, " ") == "launchctl bootout user/502/"+DaemonLabel {
			t.Fatalf("the chosen domain's own job was booted out: %v", runner.calls)
		}
	}
}
