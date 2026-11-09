//go:build darwin || linux

package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/selector"
)

// removedScenario is a signed-in computer whose daemon the server refuses (401): the real daemon, local API and
// login code run; only the machine setup steps (cleanup, install, pairing) are recorded. Wiping the setup (the fake
// install) makes the daemon accepted again, as a new pairing would.
type removedScenario struct {
	f          *loginDeviceFixture
	configPath string
	profiles   config.ProfileStore
	url        string
	steps      *[]string
	refused    *atomic.Bool
	token      *string
	paired     *string // workspace the pairing enrollment was for
	// answer is the choice made at the "Add it again?" prompt; titles records what it asked.
	answer *string
	titles *[]string
}

func newRemovedScenario(t *testing.T, f *loginDeviceFixture) removedScenario {
	t.Helper()
	srv := loginDeviceServer(t, f)
	t.Setenv("HOME", t.TempDir()) // the replacement note lives in the user config directory
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	configPath, profiles := browserLoginFixture(t, srv.URL)
	root := commandRuntimeTestRoot(t)
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", filepath.Join(root, "identity"))
	state, err := runtimeIdentityStore()
	if err != nil {
		t.Fatal(err)
	}
	key := state.Current()
	if err = state.SaveRegistration(identity.Registration{ServerURL: srv.URL, MachineID: "machine_login", EnvironmentID: "machine_login", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: root, InstallationGeneration: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	paths := localapi.Paths{StateRoot: root, RuntimeRoot: root, SocketPath: filepath.Join(root, "api.sock"), LockPath: filepath.Join(root, "daemon.lock")}
	oldPaths := currentLocalDaemonPaths
	currentLocalDaemonPaths = func() (localapi.Paths, error) { return paths, nil }
	var refused atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- localdaemon.Run(ctx, localdaemon.DaemonConfig{Paths: paths, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), RefreshInterval: time.Hour, Source: loginInventorySource(func(context.Context) ([]api.UserMachine, error) {
			if refused.Load() {
				return nil, &api.APIError{Status: 401, Code: "unauthorized"}
			}
			return nil, nil
		})})
	}()
	steps := &[]string{}
	var token, paired string
	oldTerminal, oldChoose := workspaceTerminal, chooseLoginAction
	oldMachine, oldPair, oldRemember, oldCleanup, oldSource, oldInstall := loginLocalMachineID, pairLoginDevice, rememberReplacedMachineFunc, freshEnrollmentCleanup, runningInstallSource, installSuppliedExecutable
	t.Cleanup(func() {
		cancel()
		<-done
		currentLocalDaemonPaths = oldPaths
		workspaceTerminal, chooseLoginAction = oldTerminal, oldChoose
		loginLocalMachineID, pairLoginDevice, rememberReplacedMachineFunc, freshEnrollmentCleanup, runningInstallSource, installSuppliedExecutable = oldMachine, oldPair, oldRemember, oldCleanup, oldSource, oldInstall
	})
	answer, titles := "add", &[]string{}
	workspaceTerminal = func() bool { return true }
	chooseLoginAction = func(o selector.Options) (selector.Item, error) {
		*titles = append(*titles, o.Title)
		if answer == "" { // Enter: the selector's default is the first item
			return o.Items[0], nil
		}
		for _, item := range o.Items {
			if item.ID == answer {
				return item, nil
			}
		}
		return selector.Item{}, selector.ErrCanceled
	}
	loginLocalMachineID = func(*config.Config) (string, error) {
		if len(*steps) > 0 {
			return "", nil // the setup was wiped
		}
		return "machine_login", nil
	}
	rememberReplacedMachineFunc = func() (string, error) { *steps = append(*steps, "remember"); return "machine_login", nil }
	freshEnrollmentCleanup = func(*cobra.Command) error { *steps = append(*steps, "cleanup"); return nil }
	runningInstallSource = func() (string, installsource.Source, error) { return "/tmp/pb", installsource.Source{}, nil }
	installSuppliedExecutable = func(context.Context, string, installsource.Source, string, string) (string, error) {
		*steps = append(*steps, "install")
		refused.Store(false)
		return "/tmp/pb", nil
	}
	pairLoginDevice = func(_ *command.Context, _ string, e *config.LoginEnrollment, _ string) error {
		*steps = append(*steps, "pair")
		token, paired = e.BootstrapToken, e.WorkspaceID
		return nil
	}
	waitForCommandSocket(t, paths.SocketPath)
	s := removedScenario{f: f, configPath: configPath, profiles: profiles, url: srv.URL, steps: steps, refused: &refused, token: &token, paired: &paired, answer: &answer, titles: titles}
	// The first sign-in is the one the computer later loses; the daemon accepts it.
	if out, err := runBrowserLogin(t, ctx, configPath, "login", "--no-browser"); err != nil {
		t.Fatalf("first login: %v\n%s", err, out)
	}
	f.authorize, f.enrollCalls = nil, 0
	refused.Store(true)
	return s
}

// The machine record was removed (the account lists no such machine): signing in again adds this computer back
// to the same account, once, with one approval, and says so.
func TestLoginAddsRemovedComputerBackToTheSameAccount(t *testing.T) {
	s := newRemovedScenario(t, &loginDeviceFixture{})
	// An older reset left a note naming another record; pairing would replace it, so the re-add must clear it.
	if err := bootstrap.SaveReplacement("machine_stale", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	out, err := runBrowserLogin(t, t.Context(), s.configPath, "login", "--no-browser", "--reauth")
	if err != nil {
		t.Fatalf("login --reauth: %v\n%s", err, out)
	}
	if got := strings.Join(*s.steps, ","); got != "cleanup,install,pair" || *s.token != "readd-token" || s.f.enrollCalls != 1 {
		t.Fatalf("steps=%s token=%q enrollments=%d (the removed record must not be replaced, only cleaned and added)", got, *s.token, s.f.enrollCalls)
	}
	if strings.Count(out, "Code: ABCD-EFGH") != 1 || !strings.Contains(out, "This computer was removed from browser@example.test; adding it again.") || !strings.Contains(out, "Added") {
		t.Fatalf("one approval and the message expected:\n%s", out)
	}
	if left := bootstrap.LoadReplacement(time.Now()); left != "" {
		t.Fatalf("stale replacement note %q survived the re-add", left)
	}
	if len(*s.titles) != 1 || !strings.Contains((*s.titles)[0], "This computer was removed from browser@example.test. Add it again?") {
		t.Fatalf("confirmation prompts=%v", *s.titles)
	}
	if profile, err := s.profiles.Load(s.url); err != nil || profile.Account.ID != "usr_browser" {
		t.Fatalf("sign-in not saved after the wipe: %v %+v", err, profile)
	}
}

// A machine that is still on the account is not re-added, whether personal or owned by a team (the personal
// machine list never shows team machines): the refusal keeps the --fresh advice and nothing is wiped. A lookup
// that fails without saying "no such machine" proves nothing and wipes nothing either.
func TestLoginDoesNotReaddAComputerThatIsStillOnTheAccount(t *testing.T) {
	for name, f := range map[string]*loginDeviceFixture{
		"personal":       {machines: []any{map[string]any{"id": "machine_login", "alias": "hp"}}},
		"team-owned":     {machines: []any{map[string]any{"id": "machine_login", "alias": "hp", "owner_team_id": "team_x"}}},
		"lookup failure": {lookupFail: 500},
		// A 404 is also what a live machine the account cannot see (left the team, no grant) answers.
		"invisible but alive": {invisible: true},
	} {
		t.Run(name, func(t *testing.T) { assertNotReadded(t, f) })
	}
}

func assertNotReadded(t *testing.T, f *loginDeviceFixture) {
	s := newRemovedScenario(t, f)
	out, err := runBrowserLogin(t, t.Context(), s.configPath, "login", "--no-browser", "--reauth")
	if err == nil || !strings.Contains(err.Error(), "pb login --fresh") || len(*s.steps) != 0 || s.f.enrollCalls != 0 {
		t.Fatalf("err=%v steps=%v enrollments=%d\n%s", err, *s.steps, s.f.enrollCalls, out)
	}
}

// An approval by another account is not a removal: that account does not list this computer, and login must not add it there.
func TestLoginReauthApprovedByAnotherAccountNeverReadds(t *testing.T) {
	s := newRemovedScenario(t, &loginDeviceFixture{})
	s.f.approvedBy = "other@example.test"
	out, err := runBrowserLogin(t, t.Context(), s.configPath, "login", "--no-browser", "--reauth")
	if err == nil || !strings.Contains(err.Error(), "approval came from other@example.test") || len(*s.steps) != 0 || s.f.enrollCalls != 0 {
		t.Fatalf("err=%v steps=%v enrollments=%d\n%s", err, *s.steps, s.f.enrollCalls, out)
	}
}

// The server refuses the new enrollment (plan limit): the refusal is shown, the sign-in is kept and nothing is wiped.
func TestLoginShowsTheServersRefusalToReaddARemovedComputer(t *testing.T) {
	s := newRemovedScenario(t, &loginDeviceFixture{enrollRefusal: "machine limit"})
	_, err := runBrowserLogin(t, t.Context(), s.configPath, "login", "--no-browser", "--reauth")
	if err == nil || !strings.Contains(err.Error(), "plan machine limit is reached") || len(*s.steps) != 0 {
		t.Fatalf("err=%v steps=%v", err, *s.steps)
	}
	if profile, err := s.profiles.Load(s.url); err != nil || profile.Account.ID != "usr_browser" {
		t.Fatalf("sign-in not kept: %v %+v", err, profile)
	}
}

// The browser approval signs in; it does not authorize resetting the computer. Without a yes at the prompt (declined,
// no terminal, --json) nothing is wiped and the result says computer_removed with the --fresh advice.
func TestLoginDoesNotWipeARemovedComputerWithoutAYes(t *testing.T) {
	for name, c := range map[string]struct {
		terminal bool
		answer   string
		args     []string
	}{
		"declined":    {true, "keep", nil},
		"no terminal": {false, "add", nil},
		"json":        {true, "add", []string{"--json"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newRemovedScenario(t, &loginDeviceFixture{})
			workspaceTerminal = func() bool { return c.terminal }
			*s.answer = c.answer
			out, err := runBrowserLogin(t, t.Context(), s.configPath, append([]string{"login", "--no-browser", "--reauth"}, c.args...)...)
			if err == nil || len(*s.steps) != 0 || s.f.enrollCalls != 0 {
				t.Fatalf("err=%v steps=%v enrollments=%d\n%s", err, *s.steps, s.f.enrollCalls, out)
			}
			failure := classifyCLIJSONError(err)
			if failure.Code != "computer_removed" || !strings.Contains(failure.Recovery, "pb login --fresh") || failure.StateChanged != false {
				t.Fatalf("json failure=%+v", failure)
			}
			if shown := commandFailureMessage(err, classifyCommandFailure(err)); !strings.Contains(shown, "pb login --fresh") {
				t.Fatalf("shown=%q", shown)
			}
		})
	}
}

// The workspace the user named is carried into the re-enrollment; a team that refuses this account falls back to
// Personal with a line saying so.
func TestLoginReaddsToTheNamedWorkspaceOrFallsBackToPersonal(t *testing.T) {
	for name, c := range map[string]struct {
		forbid   bool
		wantWS   string
		wantAsks []string
		wantLine bool
	}{
		"team":      {false, "myteam", []string{"myteam"}, false},
		"forbidden": {true, "personal", []string{"myteam", "personal"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			s := newRemovedScenario(t, &loginDeviceFixture{forbidTeam: c.forbid})
			out, err := runBrowserLogin(t, t.Context(), s.configPath, "login", "--no-browser", "--reauth", "--workspace", "myteam")
			if err != nil {
				t.Fatalf("login: %v\n%s", err, out)
			}
			line := strings.Contains(out, "You can no longer add computers to myteam; adding this one to Personal instead.")
			if *s.paired != c.wantWS || strings.Join(s.f.enrollWorkspaces, ",") != strings.Join(c.wantAsks, ",") || line != c.wantLine {
				t.Fatalf("paired=%q asked=%v line=%v\n%s", *s.paired, s.f.enrollWorkspaces, line, out)
			}
		})
	}
}

// The old replacement note is cleared before anything is wiped: when it cannot be cleared the working setup is left
// untouched instead of stranded half-reset.
func TestLoginReaddStopsBeforeTheWipeWhenTheStaleNoteCannotBeCleared(t *testing.T) {
	s := newRemovedScenario(t, &loginDeviceFixture{})
	note, err := bootstrap.ReplacementPath()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(note, "blocker"), 0o700); err != nil { // removing a non-empty directory fails
		t.Fatal(err)
	}
	_, err = runBrowserLogin(t, t.Context(), s.configPath, "login", "--no-browser", "--reauth")
	if err == nil || !strings.Contains(err.Error(), "replacement note") || len(*s.steps) != 0 {
		t.Fatalf("err=%v steps=%v", err, *s.steps)
	}
}
