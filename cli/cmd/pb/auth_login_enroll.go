package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/shared/machinename"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"github.com/spf13/cobra"
)

// Seams for tests; production always uses the real identity store and pairing.
var (
	loginLocalMachineID = localMachineID
	pairLoginDevice     = pairLoginEnrollment
)

// localMachineID is the machine this device already is on the configured server, or "" when it
// has not been added yet.
func localMachineID(cfg *config.Config) (string, error) {
	matches, err := localDaemonRegistrationMatches(cfg)
	if err != nil || !matches {
		return "", err
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return "", err
	}
	registration, err := store.Registration()
	if err != nil {
		return "", err
	}
	return registration.MachineID, nil
}

// loginJoin says what approving a login here means: a device that is not a machine yet joins the
// account (in the workspace given with --workspace, else the one chosen at approval); a machine
// that is already added only signs in again.
func loginJoin(cfg *config.Config, workspace string) (api.DeviceJoin, error) {
	machineID, err := loginLocalMachineID(cfg)
	if err != nil {
		return api.DeviceJoin{}, err
	}
	if machineID != "" {
		return api.DeviceJoin{EnrolledMachineID: machineID}, nil
	}
	return api.DeviceJoin{Enroll: true, Workspace: strings.TrimSpace(workspace)}, nil
}

func loginDeviceName() string {
	name, _ := os.Hostname()
	if machinename.Validate(name) != nil {
		return ""
	}
	return name
}

func workspaceLabel(id string) string {
	if id == "" || id == "personal" {
		return "Personal"
	}
	return id
}

// pairLoginEnrollment adds this device to the account with the enrollment its approved login
// carried: the same pairing `pb pair` runs with a dashboard token.
func pairLoginEnrollment(c *command.Context, server string, enrollment *config.LoginEnrollment, name string) error {
	if !serverclock.Now().Before(enrollment.ExpiresAt) {
		return errors.New("the approval to add this computer expired; run `pb login` again")
	}
	dir, err := os.MkdirTemp("", "pb-login-")
	if err != nil {
		return fmt.Errorf("prepare pairing: %w", err)
	}
	defer os.RemoveAll(dir)
	tokenFile := filepath.Join(dir, "enrollment-token")
	if err = writeLoginEnrollmentToken(tokenFile, enrollment.BootstrapToken); err != nil {
		return fmt.Errorf("prepare pairing: %w", err)
	}
	arguments := []string{"bootstrap", "--server", server, "--enrollment-token-file", tokenFile}
	if name != "" {
		arguments = append(arguments, "--name", name)
	}
	// Progress and administrator prompts reach the terminal; JSON callers get only the final result.
	var failure boundedTailBuffer
	var out io.Writer = c.Writer
	var errOut io.Writer = io.MultiWriter(c.ErrWriter, &failure)
	if c.Bool("json") {
		out, errOut = io.Discard, &failure
	}
	before, _ := configuredMachineID()
	if code := hostruntimecmd.Execute(c.Context, arguments, os.Stdin, out, errOut); code != 0 {
		created, _ := configuredMachineID()
		pairingFailed(c, before, created)
		return errors.New(runtimeFailureLine(failure.String(), "pairing failed"))
	}
	machineID, err := configuredMachineID()
	if err != nil {
		return fmt.Errorf("read paired machine registration: %w", err)
	}
	if err := finishMachineReplacement(c, machineID); err != nil {
		return err
	}
	for _, warning := range installedTrustWarnings(c.Context) {
		if _, err = fmt.Fprintln(c.ErrWriter, warning); err != nil {
			return err
		}
	}
	return nil
}

// loginDaemonAwaitingEnrollment reports whether this computer's daemon says it is not enrolled. It only reads the
// local snapshot: a missing or unreachable daemon is not that state.
var loginDaemonAwaitingEnrollment = func(ctx context.Context) bool {
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return false
	}
	client, err := localapi.NewClient(paths.SocketPath, 2*time.Second)
	if err != nil {
		return false
	}
	snapshot, err := client.Snapshot(ctx)
	return err == nil && snapshot.DaemonState == "awaiting_enrollment"
}

// errStartFresh is returned from the login prompt when the user asks to remove this computer's setup
// and add it again; the caller does that after the login lock is released.
var errStartFresh = errors.New("start fresh")

// offerLoginChoices tells a signed-in, already added computer where it stands and offers to sign in
// again or start fresh. It reports whether login should go on to a new approval.
func offerLoginChoices(c *command.Context, store config.ProfileStore, issuer string, old config.Profile, machineID string, state *config.BrowserLoginState) (bool, error) {
	who := firstNonEmpty(old.Account.Email, old.Account.DisplayName, old.Account.ID)
	if loginDaemonAwaitingEnrollment(c.Context) {
		// The sign-in is valid but this computer's service lost its enrollment; signing in again cannot restore it.
		// Approving again proves the account; login then re-adds the computer if its machine record is gone.
		state.ExpectedAccountID = old.Account.ID
		return true, nil
	}
	summary := "Already signed in as " + who
	if where := machineWorkspaceSummary(c.Context, store, issuer, machineID); where != "" {
		summary += " on " + where
	}
	summary += "."
	if c.Bool("json") || !workspaceTerminal() {
		_, err := fmt.Fprintln(c.ErrWriter, summary+" Run pb login --fresh to remove this computer's setup and add it again, or --reauth or --change-account to sign in again.")
		return false, err
	}
	choice, err := chooseLoginAction(selector.Options{Context: c.Context, Title: summary, Items: []selector.Item{
		{ID: "keep", Title: "Keep as it is"},
		{ID: "reauth", Title: "Authenticate again"},
		{ID: "change", Title: "Sign in with another account", Description: "Current login stays usable until the new sign-in succeeds"},
		{ID: "fresh", Title: "Start fresh", Description: "Remove this computer's setup and its machine record, then add it again"},
	}, Stdin: os.Stdin, Output: c.ErrWriter})
	if errors.Is(err, selector.ErrCanceled) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch choice.ID {
	case "keep":
		return false, nil
	case "reauth":
		state.ExpectedAccountID = old.Account.ID
	case "fresh":
		return false, errStartFresh
	}
	return true, nil
}

// machineWorkspaceSummary names the workspaces a machine of this account is in ("Personal",
// "Personal, Acme", "Acme (waiting for an admin)"). Empty when it cannot be read.
func machineWorkspaceSummary(ctx context.Context, store config.ProfileStore, issuer, machineID string) string {
	credential, err := store.CredentialFor(issuer)
	if err != nil {
		return ""
	}
	client := api.New(issuer, credential, loginHTTPClient())
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return ""
	}
	parts := []string{"Personal"}
	for _, machine := range machines {
		if machine.ID == machineID && machine.OwnerTeamID != "" {
			parts = []string{}
		}
	}
	teams, err := client.ListTeams(ctx)
	if err != nil {
		return strings.Join(parts, ", ")
	}
	for _, team := range teams {
		name := firstNonEmpty(team.Name, team.TeamID)
		bindings, err := client.ListTeamMachines(ctx, team.TeamID)
		if err != nil {
			return strings.Join(parts, ", ")
		}
		for _, binding := range bindings {
			if binding.MachineID == machineID && (binding.Active || binding.OwnerTeamID == team.TeamID) {
				parts = append(parts, name)
			}
		}
		for _, request := range team.MachineRequests {
			if request.MachineID == machineID && request.State == "pending" {
				parts = append(parts, name+" (waiting for an admin)")
			}
		}
	}
	return strings.Join(parts, ", ")
}

// startFresh removes this computer's local setup and reinstalls the running executable, so login can add
// the computer again from nothing; the new pairing then replaces the old machine record.
func startFresh(c *command.Context, replacesRecord bool) error {
	cobraCommand, ok := c.Context.Value(confirmationCommandKey{}).(*cobra.Command)
	if !ok {
		return errors.New("start fresh needs a CLI command context")
	}
	if replacesRecord {
		if _, err := rememberReplacedMachineFunc(); err != nil {
			return machineReplacementError{step: replaceStepRemember, cause: err}
		}
	} else if err := bootstrap.ClearReplacement(); err != nil {
		// The record is gone; an older reset's note would make pairing replace whatever it names. Cleared before
		// anything is wiped, so a failure here leaves the working setup untouched.
		return fmt.Errorf("clear the old replacement note before resetting this computer: %w", err)
	}
	if err := freshEnrollmentCleanup(cobraCommand); err != nil {
		return err
	}
	executable, source, err := runningInstallSource()
	if err != nil {
		return fmt.Errorf("reinstall after cleanup: %w", err)
	}
	ctx, cancel := context.WithTimeout(c.Context, suppliedInstallTimeout(runtime.GOOS))
	defer cancel()
	if _, err = installSuppliedExecutable(ctx, executable, source, "", selectedInstallServer(cobraCommand)); err != nil {
		return fmt.Errorf("reinstall after cleanup: %w", err)
	}
	if !replacesRecord {
		return nil
	}
	_, err = fmt.Fprintln(c.ErrWriter, "Removed this computer's setup. Adding it again; the new machine replaces the old record once it works.")
	return err
}

// machineRemovedError says the account this computer signed in to no longer has its machine record. It carries the
// sign-in and a new enrollment, so login can add the computer again once the old local setup is wiped.
type machineRemovedError struct {
	account string
	resume  config.BrowserLoginState
}

// computerRemovedError is the typed cause of a login that found the computer removed but did not wipe it, so JSON
// callers get the code computer_removed.
type computerRemovedError struct{}

func (*computerRemovedError) Error() string { return "this computer was removed from the account" }

func (e *machineRemovedError) Error() string { return "this computer was removed from " + e.account }

// readdRemovedComputer decides, after a sign-in that left the daemon refused, whether this computer's machine record
// was removed from that same account (the sign-in was approved by the expected account, and its machine list no
// longer holds the local machine). It then asks for a new enrollment in the account's personal workspace. nil means
// not that case; a server refusal (plan limit, policy) is returned to the user.
func readdRemovedComputer(c *command.Context, cfg *config.Config, store config.ProfileStore, issuer string, state *config.BrowserLoginState, refreshErr error) error {
	var remote *localapi.RemoteError
	if !errors.As(refreshErr, &remote) || remote.Code != localapi.AuthenticationRequiredCode || state.ExpectedAccountID == "" {
		return nil
	}
	machineID, err := loginLocalMachineID(cfg)
	if err != nil || machineID == "" {
		return nil
	}
	profile, err := store.Load(issuer)
	if err != nil || profile.Account.ID != state.ExpectedAccountID {
		return nil
	}
	ctx := c.Context
	credential, err := store.CredentialFor(issuer)
	if err != nil {
		return nil
	}
	client := api.New(issuer, credential, loginHTTPClient())
	// Only the server's explicit "no such machine" counts: team-owned machines are not in the personal list, and a
	// refusal or a failure to answer proves nothing, so none of those may wipe a working setup.
	if _, err = client.GetUserMachine(ctx, machineID); !api.IsMachineRemoved(err) {
		return nil
	}
	account := firstNonEmpty(profile.Account.Email, profile.Account.DisplayName, profile.Account.ID)
	// The approval authorized signing in, not resetting this computer's setup: that needs its own yes, and a
	// script or --json caller never gets to wipe.
	declined := browserLoginFailure{message: "Signed in as " + account + ", but this computer was removed from the account. Run pb login --fresh to add it again; that resets its local Paperboat setup (your Inbox is kept).", cause: &computerRemovedError{}}
	if c.Bool("json") || !workspaceTerminal() {
		return declined
	}
	choice, err := chooseLoginAction(selector.Options{Context: ctx, Title: "This computer was removed from " + account + ". Add it again?", Items: []selector.Item{
		{ID: "keep", Title: "Not now", Description: "Leaves this computer as it is"}, // first, so Enter declines
		{ID: "add", Title: "Add it again", Description: "Resets this computer's local Paperboat setup; your Inbox is kept"},
	}, Stdin: os.Stdin, Output: c.ErrWriter})
	if errors.Is(err, selector.ErrCanceled) || err == nil && choice.ID != "add" {
		return declined
	}
	if err != nil {
		return err
	}
	// The removed record's team is not kept anywhere once it is gone, so the workspace is the one the user named
	// with --workspace (else Personal). A team that no longer accepts this account falls back to Personal, said aloud.
	workspace := firstNonEmpty(strings.TrimSpace(c.String("workspace")), "personal")
	enrollment, err := client.StartMachineEnrollmentIn(ctx, newIdempotencyKey(), workspace)
	var refused *api.APIError
	if errors.As(err, &refused) && refused.Code == "workspace_forbidden" && workspace != "personal" {
		if _, err = fmt.Fprintf(c.ErrWriter, "You can no longer add computers to %s; adding this one to Personal instead.\n", workspace); err != nil {
			return err
		}
		workspace = "personal"
		enrollment, err = client.StartMachineEnrollmentIn(ctx, newIdempotencyKey(), workspace)
	}
	if err != nil {
		return browserLoginFailure{message: "Signed in as " + account + ", but this computer was removed from the account and could not be added again: " + err.Error(), cause: err}
	}
	resume := *state
	resume.Credential = &credential
	resume.Profile = &config.Profile{Issuer: issuer, Account: profile.Account, CLIClientSessionID: profile.CLIClientSessionID, AccessExpiresAt: credential.ExpiresAt}
	resume.Enrollment = &config.LoginEnrollment{BootstrapToken: enrollment.BootstrapToken, WorkspaceID: workspace, ExpiresAt: enrollment.ExpiresAt}
	return &machineRemovedError{account: account, resume: resume}
}
