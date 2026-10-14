package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/userpaths"
	"github.com/spf13/cobra"
)

var fetchCLIUpdatePolicy = api.FetchUpdatePolicy
var cliUpdateVersion = func() string { return buildinfo.Version }
var cliUpdatePolicyCachePath = cliPolicyCachePath

func updatePolicyRecoveryCommand(command *cobra.Command) bool {
	path := strings.Fields(command.CommandPath())
	if len(path) > 1 {
		switch path[1] {
		case "update", "login", "logout", "auth", "switch", "doctor", "diagnostics", "version", "help", "completion", "daemon", "install", "uninstall", "__service":
			return true
		}
		if strings.HasPrefix(path[1], "__") {
			return true
		}
	}
	switch command.Name() {
	case "cancel", "stop", "close", "delete", "remove", "disconnect", "revoke", "__complete", "__completeNoDesc":
		return true
	}
	return false
}

func checkCLIUpdatePolicy(command *cobra.Command) error {
	if buildinfo.Distribution != "official" {
		return nil
	}
	if updatePolicyRecoveryCommand(command) {
		return nil
	}
	cfg, err := config.Load(commandFlagString(command, "config"))
	if err != nil {
		return nil
	} // The owning action reports configuration errors.
	issuer := cfg.ServerURL
	if override := commandFlagString(command, "server"); override != "" {
		issuer = override
	}
	issuer, err = config.NormalizeServerURL(issuer)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(command.Context(), time.Second)
	defer cancel()
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	policy, err := fetchCLIUpdatePolicy(ctx, issuer, client)
	cachePath := cliUpdatePolicyCachePath(issuer, cfg.Path())
	if err != nil {
		policy, err = readCLIUpdatePolicy(cachePath)
		if err != nil {
			return nil
		} // The server still enforces incompatible requests.
	} else {
		_ = saveCLIUpdatePolicy(cachePath, policy)
	}
	version := cliUpdateVersion()
	if !policy.Behind(version) {
		return nil
	}
	jsonOutput, _ := command.Flags().GetBool("json")
	if policy.Required(version, time.Now()) {
		if !jsonOutput {
			fmt.Fprintf(command.ErrOrStderr(), "Update required: Paperboat %s or newer.\n", policy.MinimumVersion)
			if policy.Reason != "" {
				fmt.Fprintln(command.ErrOrStderr(), policy.Reason)
			}
		}
		return &api.APIError{Status: http.StatusUpgradeRequired, Code: "update_required", Details: map[string]any{"minimum_version": policy.MinimumVersion, "enforce_at": policy.EnforceAt}}
	}
	if !jsonOutput {
		fmt.Fprintf(command.ErrOrStderr(), "Update Paperboat before %s: version %s or newer will be required. Run `pb update`.\n", policy.EnforceAt.Local().Format(time.RFC3339), policy.MinimumVersion)
	}
	return nil
}

func commandFlagString(command *cobra.Command, name string) string {
	value, _ := command.Flags().GetString(name)
	return value
}

func cliPolicyCachePath(issuer, configPath string) string {
	root, err := userpaths.Cache("paperboat/update-policy")
	if err != nil {
		return ""
	}
	digest := sha256.Sum256([]byte(issuer + "\x00" + configPath))
	return filepath.Join(root, hex.EncodeToString(digest[:])+".json")
}

func readCLIUpdatePolicy(path string) (api.UpdatePolicy, error) {
	var policy api.UpdatePolicy
	info, err := os.Lstat(path)
	if err != nil {
		return policy, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4096 {
		return policy, errors.New("invalid cached update policy")
	}
	file, err := os.Open(path)
	if err != nil {
		return policy, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return policy, errors.New("invalid cached update policy")
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &policy) != nil {
		return policy, errors.New("invalid cached update policy")
	}
	return policy, policy.Validate()
}

func saveCLIUpdatePolicy(path string, policy api.UpdatePolicy) error {
	if path == "" || policy.Validate() != nil {
		return errors.New("invalid cached update policy")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data, atomicfile.Options{Mode: 0600, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid()})
}
