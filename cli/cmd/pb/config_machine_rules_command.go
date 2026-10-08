package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/spf13/cobra"
)

const configScopeLimit = 64 * 1024

func decodeConfigInput(r io.Reader, value any) error {
	data, err := io.ReadAll(io.LimitReader(r, configScopeLimit+1))
	if err != nil || len(data) > configScopeLimit {
		return errors.New("configuration input could not be read or exceeds 64 KiB")
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return errors.New("configuration input must be valid JSON with supported fields")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("configuration input must contain exactly one JSON value")
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

func configRepositoryCobraCommand() *cobra.Command {
	group := &cobra.Command{Use: "repository", Short: "Connect custom Git repositories and manage machine credentials"}
	list := &cobra.Command{Use: "list", Short: "List connected repositories", Args: commandArgs(cobra.NoArgs)}
	list.RunE = func(cmd *cobra.Command, args []string) error {
		client, err := backendClient(actionContext(cmd, args))
		if err != nil {
			return err
		}
		items, err := client.ListConfigRepositories(cmd.Context())
		if err != nil {
			return friendlyCommandError(err)
		}
		if jsonOutput, _ := cmd.Flags().GetBool("json"); jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"version": "1", "repositories": items})
		}
		for _, item := range items {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", item.ID, item.DisplayName, item.Provider)
		}
		return nil
	}
	list.Flags().Bool("json", false, "print JSON")
	connect := &cobra.Command{Use: "connect <url-or-absolute-path>", Short: "Connect an HTTPS, SSH, or local Git repository", Args: commandArgs(cobra.ExactArgs(1))}
	connect.Flags().String("name", "", "display name")
	connect.Flags().String("branch", "main", "Git branch")
	connect.Flags().Bool("json", false, "print JSON")
	connect.RunE = func(cmd *cobra.Command, args []string) error {
		if _, _, err := configsync.NormalizeRepositoryEndpoint(args[0]); err != nil {
			return errors.New("repository must be an HTTPS, HTTP, SSH URL or absolute local path without embedded credentials")
		}
		client, err := backendClient(actionContext(cmd, args))
		if err != nil {
			return err
		}
		name, _ := cmd.Flags().GetString("name")
		branch, _ := cmd.Flags().GetString("branch")
		repository, err := client.ConnectCustomConfigRepository(cmd.Context(), args[0], name, branch)
		if err != nil {
			return friendlyCommandError(err)
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
				return err
			}
			if repository.Provider != "git" {
				return errors.New("machine credential profiles require a custom Git repository")
			}
			root, err := configRepositoryCredentialRoot()
			if err != nil {
				return err
			}
			if cmd == set {
				input, _ := cmd.Flags().GetString("file")
				if input == "" {
					return errors.New("--file is required; use - to read stdin")
				}
				var reader io.Reader = cmd.InOrStdin()
				if input != "-" {
					data, readErr := readOwnerOnlyFile(input, configScopeLimit)
					if readErr != nil {
						return errors.New("credential input must be an owner-only file")
					}
					defer clear(data)
					reader = bytes.NewReader(data)
				}
				var profile configsync.RepositoryCredentialProfile
				if err = decodeConfigInput(reader, &profile); err != nil {
					return err
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
