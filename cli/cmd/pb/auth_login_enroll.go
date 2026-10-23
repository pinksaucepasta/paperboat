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

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/machinename"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/serverclock"
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
	if err = os.WriteFile(tokenFile, []byte(enrollment.BootstrapToken), 0o600); err != nil {
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
	if code := hostruntimecmd.Execute(c.Context, arguments, os.Stdin, out, errOut); code != 0 {
		return errors.New(runtimeFailureLine(failure.String(), "pairing failed"))
	}
	for _, warning := range installedTrustWarnings(c.Context) {
		if _, err = fmt.Fprintln(c.ErrWriter, warning); err != nil {
			return err
		}
	}
	return nil
}

// errStartFresh is returned from the login prompt when the user asks to remove this computer's setup
// and add it again; the caller does that after the login lock is released.
var errStartFresh = errors.New("start fresh")

// offerLoginChoices tells a signed-in, already added computer where it stands and offers to sign in
// again or start fresh. It reports whether login should go on to a new approval.
func offerLoginChoices(c *command.Context, store config.ProfileStore, issuer string, old config.Profile, machineID string, state *config.BrowserLoginState) (bool, error) {
	summary := "Already signed in as " + firstNonEmpty(old.Account.Email, old.Account.DisplayName, old.Account.ID)
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
		for _, binding := range team.Machines {
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

// startFresh removes this computer's local setup and its machine record, and reinstalls the running
// executable, so login can add the computer again from nothing.
func startFresh(c *command.Context) error {
	cobraCommand, ok := c.Context.Value(confirmationCommandKey{}).(*cobra.Command)
	if !ok {
		return errors.New("start fresh needs a CLI command context")
	}
	if _, err := removeOwnMachineRecordFunc(cobraCommand); err != nil {
		return err
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
	if _, err = installSuppliedExecutable(ctx, executable, source, ""); err != nil {
		return fmt.Errorf("reinstall after cleanup: %w", err)
	}
	_, err = fmt.Fprintln(c.ErrWriter, "Removed this computer's setup and its machine record. Adding it again.")
	return err
}
