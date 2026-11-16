package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	gittransport "github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"github.com/spf13/cobra"
)

const configScopeLimit = 64 * 1024

func decodeConfigInput(r io.Reader, value any) error {
	data, err := io.ReadAll(io.LimitReader(r, configScopeLimit+1))
	if err != nil {
		clear(data)
		return safeCommandFailureFor("Configuration input could not be read.", err)
	}
	if len(data) > configScopeLimit {
		clear(data)
		return localArgumentError("Configuration input exceeds 64 KiB.")
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return usageError{err: err, publicMessage: "Configuration input must be valid JSON with supported fields."}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return usageError{err: err, publicMessage: "Configuration input must contain exactly one JSON value."}
		}
		return localArgumentError("Configuration input must contain exactly one JSON value.")
	}
	return nil
}

func configRepositoryCredentialRoot() (string, error) {
	root, err := previewRuntimeStateRoot()
	if err != nil {
		return "", err
	}
	root = filepath.Join(root, "config-sync", "repository-credentials")
	if err = configsync.EnsureRepositoryCredentialRoot(root); err != nil {
		return "", err
	}
	return root, nil
}

// readConfigCredentialProfile reads a repository credential profile from an owner-only file or
// stdin ("-").
func readConfigCredentialProfile(cmd *cobra.Command, input string) (configsync.RepositoryCredentialProfile, error) {
	var profile configsync.RepositoryCredentialProfile
	var reader io.Reader = cmd.InOrStdin()
	if input != "-" {
		data, err := readOwnerOnlyFile(input, configScopeLimit)
		if err != nil {
			return profile, userInputFileError("Credential input must be an owner-only file.", err)
		}
		defer clear(data)
		reader = bytes.NewReader(data)
	}
	return profile, decodeConfigInput(reader, &profile)
}

// checkConfigRepository reads the repository the way a machine will before it is connected: it
// must be reachable with these credentials, have the branch with a commit, and any
// .paperboat/config-sync.toml on it must be a valid config sync file. HTTP(S) and local
// repositories can be checked anonymously; SSH needs a credential profile.
func checkConfigRepository(ctx context.Context, stateDir, endpoint, transport, branch string, profile *configsync.RepositoryCredentialProfile) error {
	if profile == nil && transport == "ssh" {
		return configSyncRefusal("Checking an SSH repository needs its credentials: pass --credentials-file <profile> (the JSON `pb config repository credentials set` reads).")
	}
	dir, err := os.MkdirTemp(stateDir, "connect-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	const id = "connect-check"
	credentials := filepath.Join(dir, "credentials")
	if err := configsync.EnsureRepositoryCredentialRoot(credentials); err != nil {
		return err
	}
	if transport != "local" {
		p := configsync.RepositoryCredentialProfile{Transport: transport, Auth: "anonymous", AllowInsecureHTTP: transport == "http"}
		if profile != nil {
			p = *profile
		}
		if err := configsync.SaveRepositoryCredentialProfile(credentials, id, p); err != nil {
			return usageError{err: err, publicMessage: "The credential profile does not fit this repository: check transport, auth and the files it names."}
		}
	}
	access := configsync.RepositoryReadAccess{RepositoryID: id, CloneURL: endpoint, Branch: branch, Transport: transport, Capability: "repository_contents_read", ExpiresAt: serverclock.Now().Add(2 * time.Minute)}
	if _, _, err := configsync.ReadSharedRepositoryConfig(ctx, filepath.Join(dir, "read"), credentials, access, configsync.DefaultSourceConfigLimits()); err != nil {
		if errors.Is(err, gittransport.ErrAuthenticationRequired) || errors.Is(err, gittransport.ErrAuthorizationFailed) || errors.Is(err, gittransport.ErrRepositoryNotFound) || profile == nil && errors.Is(err, configsync.ErrRepositoryCredentials) {
			return configSyncRefusal("The repository was not found or refused these credentials. Check the URL, or pass --credentials-file <profile> (the JSON `pb config repository credentials set` reads) for a private repository.")
		}
		return configRepositoryReadFailure(err)
	}
	return nil
}

func configRepositoryCobraCommand() *cobra.Command {
	group := &cobra.Command{Use: "repository", Short: "Connect custom Git repositories and manage machine credentials"}
	list := &cobra.Command{Use: "list", Short: "List connected repositories", Args: commandArgs(cobra.NoArgs)}
	list.RunE = func(cmd *cobra.Command, args []string) error {
		client, err := backendClient(actionContext(cmd, args))
		if err != nil {
			return err
		}
		maxItems, err := listPagingMax(cmd)
		if err != nil {
			return err
		}
		items, total, err := client.ListConfigRepositoriesPage(cmd.Context(), inventoryFilters(cmd), maxItems)
		if err != nil {
			return friendlyCommandError(err)
		}
		if jsonOutput, _ := cmd.Flags().GetBool("json"); jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"version": "1", "repositories": items, "total": total})
		}
		defer writeListFooter(cmd.ErrOrStderr(), len(items), total, "repositories")
		for _, item := range items {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", item.ID, item.DisplayName, item.Provider)
		}
		return nil
	}
	list.Flags().Bool("json", false, "print JSON")
	inventoryFilterFlags(list)
	addListPaging(list)
	connect := &cobra.Command{Use: "connect <url-or-absolute-path>", Short: "Check and connect an HTTPS, SSH, or local Git repository", Args: commandArgs(cobra.ExactArgs(1))}
	connect.Flags().String("name", "", "display name")
	connect.Flags().String("branch", "main", "Git branch")
	connect.Flags().String("credentials-file", "", "private credential profile used to check the repository and saved on this machine; - for stdin (required for SSH)")
	connect.Flags().Bool("json", false, "print JSON")
	connect.RunE = func(cmd *cobra.Command, args []string) error {
		endpoint, transport, err := configsync.NormalizeRepositoryEndpoint(args[0])
		if err != nil {
			return localArgumentError("Repository must be an HTTPS, HTTP, SSH URL or absolute local path without embedded credentials.")
		}
		client, err := backendClient(actionContext(cmd, args))
		if err != nil {
			return err
		}
		name, _ := cmd.Flags().GetString("name")
		branch, _ := cmd.Flags().GetString("branch")
		input, _ := cmd.Flags().GetString("credentials-file")
		var profile *configsync.RepositoryCredentialProfile
		if input != "" {
			p, err := readConfigCredentialProfile(cmd, input)
			if err != nil {
				return err
			}
			profile = &p
		}
		root, err := configRepositoryCredentialRoot()
		if err != nil {
			return err
		}
		if err := checkConfigRepository(cmd.Context(), filepath.Dir(root), endpoint, transport, branch, profile); err != nil {
			return err
		}
		repository, err := client.ConnectCustomConfigRepository(cmd.Context(), args[0], name, branch)
		if err != nil {
			return friendlyCommandError(err)
		}
		if profile != nil {
			if err := configsync.SaveRepositoryCredentialProfile(root, repository.ID, *profile); err != nil {
				return err
			}
		}
		if output, _ := cmd.Flags().GetBool("json"); output {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"version": "1", "repository": repository})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Connected %s (%s). Define explicit machine path rules before syncing files.\n", repository.DisplayName, repository.ID)
		return nil
	}
	credentials := &cobra.Command{Use: "credentials", Short: "Store credentials privately on this machine"}
	set := &cobra.Command{Use: "set <repository>", Short: "Read a credential profile from a private JSON file or stdin", Args: commandArgs(cobra.ExactArgs(1))}
	set.Flags().String("file", "", "private profile file; use - for stdin")
	set.Flags().Bool("json", false, "print JSON")
	remove := &cobra.Command{Use: "remove <repository>", Short: "Remove this machine's saved repository credentials", Args: commandArgs(cobra.ExactArgs(1))}
	remove.Flags().Bool("json", false, "print JSON")
	for _, cmd := range []*cobra.Command{set, remove} {
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			client, err := backendClient(actionContext(cmd, args))
			if err != nil {
				return err
			}
			items, err := client.ListConfigRepositories(cmd.Context())
			if err != nil {
				return friendlyCommandError(err)
			}
			repository, err := resolveConfigRepository(items, args[0])
			if err != nil {
				return usageError{err: err, publicMessage: "Select a connected repository with `pb config repository list`."}
			}
			if repository.Provider != "git" {
				return localArgumentError("Machine credential profiles require a custom Git repository.")
			}
			root, err := configRepositoryCredentialRoot()
			if err != nil {
				return err
			}
			if cmd == set {
				input, _ := cmd.Flags().GetString("file")
				if input == "" {
					return localArgumentError("--file is required; use - to read stdin.")
				}
				profile, readErr := readConfigCredentialProfile(cmd, input)
				if readErr != nil {
					return readErr
				}
				err = configsync.SaveRepositoryCredentialProfile(root, repository.ID, profile)
			} else {
				err = configsync.DeleteRepositoryCredentialProfile(root, repository.ID)
			}
			if err != nil {
				return err
			}
			if output, _ := cmd.Flags().GetBool("json"); output {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"version": "1", "repository_id": repository.ID, "state": map[bool]string{true: "saved", false: "removed"}[cmd == set]})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Repository credentials %s on this machine.\n", map[bool]string{true: "saved", false: "removed"}[cmd == set])
			return nil
		}
	}
	credentials.AddCommand(set, remove)
	group.AddCommand(list, connect, credentials)
	return group
}
