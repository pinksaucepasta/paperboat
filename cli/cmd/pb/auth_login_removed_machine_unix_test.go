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
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
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
}

func newRemovedScenario(t *testing.T, f *loginDeviceFixture) removedScenario {
	t.Helper()
	srv := loginDeviceServer(t, f)
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
	var token string
	oldMachine, oldPair, oldRemember, oldCleanup, oldSource, oldInstall := loginLocalMachineID, pairLoginDevice, rememberReplacedMachineFunc, freshEnrollmentCleanup, runningInstallSource, installSuppliedExecutable
	t.Cleanup(func() {
		cancel()
		<-done
		currentLocalDaemonPaths = oldPaths
		loginLocalMachineID, pairLoginDevice, rememberReplacedMachineFunc, freshEnrollmentCleanup, runningInstallSource, installSuppliedExecutable = oldMachine, oldPair, oldRemember, oldCleanup, oldSource, oldInstall
	})
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
		token = e.BootstrapToken
		return nil
	}
	waitForCommandSocket(t, paths.SocketPath)
	s := removedScenario{f: f, configPath: configPath, profiles: profiles, url: srv.URL, steps: steps, refused: &refused, token: &token}
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
	if profile, err := s.profiles.Load(s.url); err != nil || profile.Account.ID != "usr_browser" {
		t.Fatalf("sign-in not saved after the wipe: %v %+v", err, profile)
	}
}

// A machine that is still on the account is not re-added: the refusal keeps the --fresh advice.
func TestLoginDoesNotReaddAComputerThatIsStillOnTheAccount(t *testing.T) {
	s := newRemovedScenario(t, &loginDeviceFixture{machines: []any{map[string]any{"id": "machine_login", "alias": "hp"}}})
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
