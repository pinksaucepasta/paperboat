package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/recoverykey"
	"github.com/spf13/cobra"
)

func privilegedServiceOperationCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "__runtime-service",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(command *cobra.Command, args []string) error {
			code := hostruntimecmd.Execute(command.Context(), append([]string{"service"}, args...), command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr())
			if code != 0 {
				return exitCodeError{code: code}
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}

func pairCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "pair",
		Short: "Enroll this machine with a one-shot token",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			jsonOutput, _ := command.Flags().GetBool("json")
			stateRoot, err := command.Flags().GetString("state-root")
			if err != nil {
				return err
			}
			if stateRoot == "" {
				stateRoot = os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
			}
			if stateRoot == "" {
				stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
				if err != nil {
					return err
				}
			}
			identityStore, err := identity.Open(identity.Config{StateRoot: stateRoot})
			if err != nil {
				return fmt.Errorf("open machine identity: %w", err)
			}
			registration, err := identityStore.Registration()
			fresh := onlyNotExistCause(err)
			if fresh {
				registration = identity.Registration{}
			}
			if err != nil {
				if fresh {
					err = nil
				} else {
					return fmt.Errorf("load machine registration: %w", err)
				}
			}
			serverURL, err := command.Flags().GetString("server")
			if err != nil {
				return err
			}
			if serverURL != "" {
				serverURL, err = config.NormalizeServerURL(serverURL)
				if err != nil {
					return err
				}
				if !fresh && serverURL != registration.ServerURL {
					return errors.New("this machine is set up for a different Paperboat server")
				}
			}
			if fresh && serverURL == "" {
				serverURL = strings.TrimSpace(buildinfo.DefaultServerURL)
			}
			if fresh && serverURL == "" {
				return localArgumentError("fresh pairing requires --server")
			}
			if fresh {
				token, _ := command.Flags().GetString("enrollment-token")
				tokenFile, _ := command.Flags().GetString("enrollment-token-file")
				if strings.TrimSpace(token) == "" && strings.TrimSpace(tokenFile) == "" {
					return localArgumentError("fresh pairing requires --enrollment-token or --enrollment-token-file")
				}
			}
			publicIdentityKey := base64.RawURLEncoding.EncodeToString(identityStore.Current().Public())
			if !fresh && registration.PublicIdentityKey != publicIdentityKey {
				return errors.New("machine setup identity does not match the current key; run `pb setup` to repair it")
			}
			if !fresh {
				serverURL = registration.ServerURL
			}
			arguments := []string{"bootstrap", "--server", serverURL}
			for _, name := range []string{"enrollment-token", "enrollment-token-file", "name", "shell", "state-root"} {
				value, err := command.Flags().GetString(name)
				if err != nil {
					return err
				}
				if strings.TrimSpace(value) != "" {
					arguments = append(arguments, "--"+name, value)
				}
			}
			operationOut, operationErr := command.OutOrStdout(), command.ErrOrStderr()
			var failureOutput boundedTailBuffer
			if jsonOutput {
				operationOut, operationErr = io.Discard, &failureOutput
			}
			code := hostruntimecmd.Execute(command.Context(), arguments, command.InOrStdin(), operationOut, operationErr)
			if code != 0 {
				if jsonOutput {
					// The runtime writes one curated "pb: ..." failure line; JSON
					// callers need that cause instead of a generic native exit.
					return jsonFailureExitCodeError{code: code, message: runtimeFailureLine(failureOutput.String(), "Machine pairing failed.")}
				}
				return exitCodeError{code: code}
			}
			warnings := installedTrustWarnings(command.Context())
			if jsonOutput {
				paired, err := identityStore.Registration()
				if err != nil {
					return fmt.Errorf("read paired machine registration: %w", err)
				}
				result := map[string]any{"machine_id": paired.MachineID, "environment_id": paired.EnvironmentID, "server_url": paired.ServerURL, "installation_generation": paired.InstallationGeneration}
				if len(warnings) > 0 {
					result["browser_https"] = "trust_pending"
					result["warnings"] = warnings
				}
				return writeCLIJSON(command.OutOrStdout(), result)
			}
			for _, warning := range warnings {
				if _, err := fmt.Fprintln(command.ErrOrStderr(), warning); err != nil {
					return err
				}
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	command.Flags().String("enrollment-token", "", "single-use pairing token")
	command.Flags().String("enrollment-token-file", "", "absolute protected file containing a single-use pairing token")
	command.Flags().String("name", "", "machine name")
	command.Flags().String("shell", "", "absolute login shell")
	command.Flags().String("state-root", "", "runtime state directory")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func setupCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "setup",
		Short: "Set up this machine for Paperboat",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			jsonOutput, _ := command.Flags().GetBool("json")
			operationOut, operationErr := command.OutOrStdout(), command.ErrOrStderr()
			if jsonOutput {
				operationOut, operationErr = io.Discard, io.Discard
			}
			var err error
			if runtime.GOOS == "windows" {
				sshPort, setupErr := setupPlatformHostPrerequisites(command.Context())
				if setupErr != nil {
					return fmt.Errorf("prepare Windows OpenSSH: %w", setupErr)
				}
				if sshPort != 0 {
					if err := command.Flags().Set("ssh-port", strconv.Itoa(int(sshPort))); err != nil {
						return err
					}
				}
			}
			sshPortValue := uint(0)
			sshPortValue, err = command.Flags().GetUint("ssh-port")
			if err != nil || sshPortValue == 0 || sshPortValue > 65535 {
				return invocationError(errors.New("--ssh-port must be between 1 and 65535"))
			}

			account, err := user.Current()
			if err != nil || strings.TrimSpace(account.Username) == "" {
				return commandPreparationFailure{step: prepareMachineUser, cause: err}
			}
			ctx := actionContext(command, nil)
			client, err := setupBackendClient(ctx)
			if err != nil {
				return err
			}
			stateRoot, err := command.Flags().GetString("state-root")
			if err != nil {
				return err
			}
			if stateRoot == "" {
				stateRoot = os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
			}
			if stateRoot == "" {
				stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
				if err != nil {
					return err
				}
			}
			identityStore, err := identity.Open(identity.Config{StateRoot: stateRoot})
			if err != nil {
				return fmt.Errorf("open machine identity: %w", err)
			}
			workspaceRoot, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve workspace root: %w", err)
			}
			workspaceRoot = filepath.Clean(workspaceRoot)
			name, err := command.Flags().GetString("name")
			if err != nil {
				return err
			}
			if strings.TrimSpace(name) == "" {
				name, err = os.Hostname()
				if err != nil || strings.TrimSpace(name) == "" {
					return commandPreparationFailure{step: prepareMachineName, cause: err}
				}
			}
			key := identityStore.Current()
			publicIdentityKey := base64.RawURLEncoding.EncodeToString(key.Public())
			inboxPath, err := inbox.DefaultPath()
			if err != nil {
				return fmt.Errorf("resolve Paperboat Inbox: %w", err)
			}
			previousRegistration, previousRegistrationErr := identityStore.Registration()
			hadPreviousRegistration := previousRegistrationErr == nil
			if previousRegistrationErr != nil && !errors.Is(previousRegistrationErr, os.ErrNotExist) {
				return fmt.Errorf("load machine registration: %w", previousRegistrationErr)
			}
			if hadPreviousRegistration {
				inboxPath = previousRegistration.InboxPath
			}
			if err := inbox.EnsurePath(inboxPath); err != nil {
				return fmt.Errorf("prepare Paperboat Inbox: %w", err)
			}
			d, err := buildDeps(ctx)
			if err != nil {
				return err
			}
			if hadPreviousRegistration && (previousRegistration.ServerURL != d.cfg.ServerURL || previousRegistration.PublicIdentityKey != publicIdentityKey) {
				return errors.New("existing machine registration belongs to a different server or identity; setup cannot replace it")
			}
			machine, err := client.SetupMachine(command.Context(), api.MachineSetupInput{
				Alias: strings.TrimSpace(name), Platform: runtime.GOOS, Architecture: runtime.GOARCH,
				WorkspaceRoot: workspaceRoot, PublicIdentityKey: publicIdentityKey,
				RuntimeVersions: map[string]string{"pb": buildinfo.Version},
			})
			if err != nil {
				if errors.Is(err, api.ErrUnauthenticated) {
					return err
				}
				return setupFailure(err)
			}
			var priorRegistration *identity.Registration
			if hadPreviousRegistration {
				priorRegistration = &previousRegistration
			}
			if err := validateSetupMachineIdentity(machine, priorRegistration, publicIdentityKey); err != nil {
				return setupFailure(err)
			}
			registration := identity.Registration{
				ServerURL: d.cfg.ServerURL, MachineID: machine.ID, EnvironmentID: machine.EnvironmentID,
				PublicKeyID: key.ID, PublicIdentityKey: publicIdentityKey,
				InboxPath:              inboxPath,
				InstallationGeneration: machine.InstallationGeneration,
				UpdatedAt:              time.Now().UTC(),
			}
			if hadPreviousRegistration {
				registration.AccountID = previousRegistration.AccountID
				registration.SSHUser, registration.SSHPort = previousRegistration.SSHUser, previousRegistration.SSHPort
			}

			if err := identityStore.SaveRegistration(registration); err != nil {
				return setupFailure(fmt.Errorf("save machine registration: %w", err))
			}
			if err := saveSetupMachineControl(command.Context(), client, identityStore, machine, key.ID); err != nil {
				return setupFailure(err)
			}
			desiredCapabilities := machine.MachineCapabilities.Desired
			for flag, value := range map[string]*bool{
				"terminal":       &desiredCapabilities.Terminal,
				"managed-ssh":    &desiredCapabilities.ManagedSSH,
				"file-receive":   &desiredCapabilities.FileReceive,
				"preview-tunnel": &desiredCapabilities.PreviewTunnel,
			} {
				if command.Flags().Changed(flag) {
					*value, _ = command.Flags().GetBool(flag)
				}
			}
			if _, err := client.SetUserMachineCapabilities(command.Context(), machine.ID, newIdempotencyKey(), desiredCapabilities, machine.MachineCapabilities.DesiredVersion); err != nil {
				return setupFailure(fmt.Errorf("save incoming machine capabilities: %w", err))
			}
			if machine.Installation == nil {
				return setupFailure(errors.New("server did not return installation material"))
			}
			if err := verifySetupInstallSource(); err != nil {
				return setupFailure(fmt.Errorf("verify supplied installation executable: %w", err))
			}
			if _, err := client.RegisterManagedSSHTarget(command.Context(), machine.ID, uint64(machine.InstallationGeneration), account.Username, uint16(sshPortValue), newIdempotencyKey()); err != nil {
				return setupFailure(fmt.Errorf("register SSH target: %w", err))
			}
			registration.SSHUser, registration.SSHPort = account.Username, uint16(sshPortValue)
			if err := identityStore.SaveRegistration(registration); err != nil {
				return setupFailure(fmt.Errorf("save registered SSH target: %w", err))
			}
			resume, err := bootstrap.PrepareAuthenticatedSetupResume(stateRoot, d.cfg.ServerURL, publicIdentityKey, strings.TrimSpace(name), machine.ID, machine.InstallationGeneration, time.Now().UTC())
			if err != nil {
				if errors.Is(err, bootstrap.ErrResumeBinding) {
					return fmt.Errorf("machine setup cannot replace the protected unfinished installation: %w; run `pb bootstrap --state-root %s` to recover that installation before starting setup again", err, stateRoot)
				}
				return setupFailure(fmt.Errorf("prepare authenticated machine setup recovery: %w", err))
			}
			reusableIdentity, reusableIdentityErr := enrollment.LoadRuntimeIdentityForRenewal(stateRoot, time.Now().UTC())
			prepared, err := client.PrepareAuthenticatedMachineInstallation(command.Context(), machine.ID, resume.SetupOperationID, api.AuthenticatedMachineInstallationInput{
				Verifier: resume.Verifier, PublicIdentityKey: publicIdentityKey,
				InstallationGeneration: machine.InstallationGeneration,
				SSHUser:                account.Username, SSHPort: uint16(sshPortValue),
				CanReuseRuntimeIdentity: reusableIdentityErr == nil && reusableIdentity.MachineID == machine.ID && reusableIdentity.EnvironmentID == machine.ID,
			})
			if err != nil {
				return setupFailure(fmt.Errorf("prepare authenticated machine installation: %w", err))
			}
			resume.PairingStarted = true
			resume.PairingExpiresAt = prepared.ExpiresAt
			if err := bootstrap.SaveResume(stateRoot, resume); err != nil {
				return setupFailure(fmt.Errorf("persist authenticated machine installation: %w", err))
			}

			arguments := []string{"bootstrap", "--server", d.cfg.ServerURL, "--state-root", stateRoot, "--name", strings.TrimSpace(name)}
			if code := hostruntimecmd.Execute(command.Context(), arguments, command.InOrStdin(), operationOut, operationErr); code != 0 {
				return setupFailure(exitCodeError{code: code})
			}
			return finishMachineSetup(command, client, identityStore, machine.ID, inboxPath, jsonOutput)
		},
		SilenceUsage: true, SilenceErrors: true,
	}
	command.Flags().String("name", "", "machine name")
	command.Flags().String("state-root", "", "runtime state directory")
	command.Flags().Uint("ssh-port", 22, "existing loopback sshd port")
	command.Flags().Bool("terminal", true, "accept Paperboat terminal and exec")
	command.Flags().Bool("managed-ssh", true, "accept managed SSH tools")
	command.Flags().Bool("file-receive", true, "accept native Inbox transfers")
	command.Flags().Bool("preview-tunnel", true, "serve previews and tunnels")
	command.Flags().String("recovery-output", "", "new absolute file for the account recovery key")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

// setupFailure keeps the unified machine and its durable setup operation intact.
// Installation owns restoration of a previously working service; retry resumes pairing.
func setupFailure(cause error) error {
	return setupFailureError{cause: cause}
}

func saveSetupMachineControl(ctx context.Context, client *api.Client, identityStore *identity.Store, machine api.UserMachine, keyID string) error {
	operationID := newIdempotencyKey()
	controlBody, err := json.Marshal(struct {
		OperationID string `json:"operation_id"`
	}{operationID})
	if err != nil {
		return err
	}
	controlPath := "/v1/machines/" + machine.ID + "/control-credentials"
	proof, err := identityStore.MachineProof(operationID, http.MethodPost, controlPath, controlBody, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("prove machine identity: %w", err)
	}
	controlCredential, err := client.IssueMachineControlCredential(ctx, machine.ID, operationID, proof)
	if err != nil {
		return fmt.Errorf("issue machine control credential: %w", err)
	}
	if err := identityStore.SaveMachineControl(identity.MachineControl{
		MachineID: machine.ID, EnvironmentID: machine.EnvironmentID,
		InstallationGeneration: machine.InstallationGeneration, Credential: controlCredential.Credential,
		ExpiresAt: controlCredential.ExpiresAt, KeyID: keyID,
	}); err != nil {
		return fmt.Errorf("save machine control credential: %w", err)
	}
	return nil
}

func waitForAvailabilityObservation(ctx context.Context, client *api.Client, machineID string, current api.AvailabilityPolicy, timeout time.Duration) api.AvailabilityPolicy {
	if current.Status == "applied" && current.ObservedVersion == current.DesiredVersion && current.ObservedMode == current.DesiredMode {
		return current
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return current
		case <-ticker.C:
			machines, err := client.ListUserMachines(waitCtx)
			if err != nil {
				continue
			}
			for _, machine := range machines {
				if machine.ID != machineID || machine.Availability.DesiredVersion != current.DesiredVersion {
					continue
				}
				current = machine.Availability
				if current.Status == "applied" || current.Status == "unsupported" || current.Status == "error" || current.Status == "offline" {
					return current
				}
			}
		}
	}
}

func exportSetupRecoveryKey(command *cobra.Command) error {
	output, err := command.Flags().GetString("recovery-output")
	if err != nil || strings.TrimSpace(output) == "" {
		return err
	}
	output = strings.TrimSpace(output)
	if !filepath.IsAbs(output) {
		return invocationError(errors.New("--recovery-output must be an absolute path"))
	}
	ctx := actionContext(command, nil)
	_, store, profile, err := e2eeClient(ctx)
	if err != nil {
		return fmt.Errorf("machine setup completed, but recovery-key export failed: %w", err)
	}
	seed, err := store.ExportPeerAccountRootSeed(profile.Issuer, profile.Account.ID)
	if err != nil {
		return fmt.Errorf("machine setup completed, but recovery-key export failed: %w", err)
	}
	defer clear(seed)
	encoded, err := recoverykey.Encode(seed)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("machine setup completed, but create recovery-key file: %w", err)
	}
	if _, err = io.WriteString(file, encoded+"\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("machine setup completed, but write recovery-key file: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("machine setup completed, but close recovery-key file: %w", err)
	}
	if !jsonOutputRequested(command) {
		fmt.Fprintf(command.OutOrStdout(), "Recovery key written to %s\n", output)
	}
	return nil
}
