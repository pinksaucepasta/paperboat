package main

import (
	"crypto/x509"
	"errors"
	"fmt"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
)

// configureRepositoryCredentialsInteractive stores secrets only on this machine,
// under the authoritative connected repository ID. It never changes its URL.
func configureRepositoryCredentialsInteractive(command *cobra.Command, repository api.ConfigRepository) error {
	if repository.ID == "" || repository.Provider != "git" {
		return errors.New("choose a connected custom Git repository before setting its credentials")
	}
	_, kind, err := configsync.NormalizeRepositoryEndpoint(repository.ExternalRef)
	if err != nil {
		return errors.New("the connected repository endpoint is invalid; reconnect it with a credential-free URL")
	}
	if kind == "local" {
		return showHomeText(command, "Local repository access", "This machine uses the repository's existing filesystem permissions. No credential profile is needed. A repository on another machine requires an explicit mounted location or an SSH/HTTPS repository binding.")
	}
	profile := configsync.RepositoryCredentialProfile{Transport: kind}
	if kind == "http" {
		confirmed, err := prompt.Confirm(prompt.ConfirmOptions{Context: command.Context(), Title: "Allow unencrypted HTTP for this repository?", Description: "HTTP exposes configuration contents and any credentials to the network. Use HTTPS or SSH for encrypted access. This choice applies only to this machine's profile.", Stdin: credentialInteractiveInput(command), Output: command.ErrOrStderr()})
		if err != nil {
			return err
		}
		if !confirmed {
			return prompt.ErrCanceled
		}
		profile.AllowInsecureHTTP = true
	}
	switch kind {
	case "https", "http":
		choice, err := chooseHomeAction(command, "Repository authentication", []selector.Item{{ID: "token", Title: "Access token", Description: "Token and the username required by your Git server"}, {ID: "basic", Title: "Username and password", Description: "Basic authentication over this repository's transport"}, {ID: "anonymous", Title: "No authentication", Description: "Use access allowed by the repository server"}})
		if err != nil {
			return err
		}
		profile.Auth = choice.ID
		if profile.Auth != "anonymous" {
			profile.Username, err = credentialInteractiveText(command, "Git server username", "Enter the username required by your server; it is not inferred from your Paperboat account.", nil)
			if err != nil {
				return err
			}
			title := "Repository password"
			if profile.Auth == "token" {
				title = "Repository access token"
			}
			profile.Password, err = credentialInteractiveSecret(command, title, "The value is hidden and saved only in this machine's private credential profile.", false)
			if err != nil {
				return err
			}
		}
		if kind == "https" {
			choice, err := chooseHomeAction(command, "HTTPS certificate trust", []selector.Item{{ID: "system", Title: "System certificate authorities", Description: "Verify the server using this machine's trusted certificate authorities"}, {ID: "custom", Title: "Additional CA certificate file", Description: "Use an explicit local PEM CA bundle; TLS verification stays enabled"}})
			if err != nil {
				return err
			}
			if choice.ID == "custom" {
				profile.CAFile, err = credentialInteractiveText(command, "CA certificate file", "Absolute path to an existing PEM CA bundle on this machine.", validateCredentialInteractiveFile)
				if err != nil {
					return err
				}
			}
		}
	case "ssh":
		choice, err := chooseHomeAction(command, "SSH authentication", []selector.Item{{ID: "key", Title: "Private key file", Description: "Use an existing private key on this machine"}, {ID: "agent", Title: "SSH agent", Description: "Use keys already loaded into this machine's SSH agent"}})
		if err != nil {
			return err
		}
		profile.Auth = "ssh"
		profile.SSHAgent = choice.ID == "agent"
		if !profile.SSHAgent {
			profile.SSHKeyFile, err = credentialInteractiveText(command, "SSH private key file", "Absolute path to an existing permission-protected private key on this machine.", validateCredentialInteractiveFile)
			if err != nil {
				return err
			}
			profile.SSHKeyPassphrase, err = credentialInteractiveSecret(command, "SSH key passphrase", "The value is hidden. Press Enter if the key has no passphrase.", true)
			if err != nil {
				return err
			}
		}
		profile.KnownHostsFile, err = credentialInteractiveText(command, "SSH known_hosts file", "Absolute path to a known_hosts file containing the server's verified host key. Unknown or changed keys are rejected.", validateCredentialInteractiveFile)
		if err != nil {
			return err
		}
	default:
		return errors.New("this repository transport does not support credential setup")
	}
	if err := validateCredentialInteractiveProfileReferences(profile); err != nil {
		return err
	}
	root, err := configRepositoryCredentialRoot()
	if err != nil {
		return err
	}
	if err := configsync.EnsureRepositoryCredentialRoot(root); err != nil {
		return err
	}
	if err := configsync.SaveRepositoryCredentialProfile(root, repository.ID, profile); err != nil {
		return fmt.Errorf("save machine repository credentials: %w", err)
	}
	return showHomeText(command, "Repository credentials saved", "This repository's credentials are stored privately on this machine. Other machines need their own credentials. Define explicit path rules before syncing files.")
}

func credentialInteractiveInput(command *cobra.Command) *os.File {
	if file, ok := command.InOrStdin().(*os.File); ok {
		return file
	}
	return os.Stdin
}
func credentialInteractiveText(command *cobra.Command, title, description string, validate func(string) error) (string, error) {
	if validate == nil {
		validate = func(value string) error {
			if value == "" || len(value) > 1024 {
				return errors.New("enter a non-empty username of at most 1024 bytes")
			}
			return nil
		}
	}
	return prompt.Text(prompt.TextOptions{Context: command.Context(), Title: title, Description: description, Stdin: credentialInteractiveInput(command), Output: command.ErrOrStderr(), Validate: validate})
}
func credentialInteractiveSecret(command *cobra.Command, title, description string, emptyAllowed bool) (string, error) {
	bytes, err := prompt.Secret(prompt.SecretOptions{Context: command.Context(), Title: title, Description: description, Stdin: credentialInteractiveInput(command), Output: command.ErrOrStderr(), MaxBytes: 16384})
	if err != nil {
		return "", err
	}
	defer clear(bytes)
	if len(bytes) == 0 && !emptyAllowed {
		return "", errors.New("a non-empty repository password or token is required; no credentials were saved")
	}
	return string(bytes), nil
}
func validateCredentialInteractiveFile(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("enter an absolute file path on this machine")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("choose an existing file through its real absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return errors.New("choose a regular file no larger than 1 MiB that other users cannot modify")
	}
	return nil
}

func validateCredentialInteractiveProfileReferences(profile configsync.RepositoryCredentialProfile) error {
	if profile.CAFile != "" {
		data, err := readCredentialInteractiveReference(profile.CAFile)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return errors.New("the CA file must contain PEM certificates; no credentials were saved")
		}
	}
	if profile.KnownHostsFile != "" {
		if err := validateCredentialInteractiveFile(profile.KnownHostsFile); err != nil {
			return err
		}
		if _, err := knownhosts.New(profile.KnownHostsFile); err != nil {
			return errors.New("the known_hosts file is invalid; use verified SSH host key entries")
		}
	}
	if profile.SSHKeyFile != "" {
		info, err := os.Lstat(profile.SSHKeyFile)
		if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
			return errors.New("SSH private key must be accessible only to its owner")
		}
		data, err := readCredentialInteractiveReference(profile.SSHKeyFile)
		if err != nil {
			return err
		}
		defer clear(data)
		if _, err := gitssh.NewPublicKeys("git", data, profile.SSHKeyPassphrase); err != nil {
			return errors.New("the SSH private key could not be unlocked; check the key file and passphrase")
		}
	}
	return nil
}
func readCredentialInteractiveReference(path string) ([]byte, error) {
	if err := validateCredentialInteractiveFile(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("credential reference file is unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("credential reference file is unavailable or exceeds 1 MiB")
	}
	return data, nil
}
