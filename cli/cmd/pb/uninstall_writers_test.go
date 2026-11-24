//go:build !windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/bootstrap"
	helperconfig "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/cli/internal/preferences"
	"github.com/pinksaucepasta/paperboat/cli/internal/recipekey"
	"github.com/pinksaucepasta/paperboat/cli/internal/userpaths"
	"github.com/spf13/cobra"
)

// What the real writers create in a home goes, the user's own files and links next to it stay.
func TestUninstallRemovesEverythingTheWritersCreated(t *testing.T) {
	home := isolatedReplacementNote(t)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "run"))
	t.Setenv("PAPERBOAT_CONFIG", "")
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", "")
	xdg.Reload()
	t.Cleanup(xdg.Reload)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	configPath, err := config.DefaultPath()
	must(err)
	cfg, err := config.Load(configPath)
	must(err)
	must(cfg.Save())
	must(preferences.Save(filepath.Join(filepath.Dir(configPath), "config.preferences.json"), preferences.Default()))
	must(recipekey.Save(configPath, recipekey.File{TokenID: "t", PrivateKey: []byte("k"), Machine: []byte("m")}))
	must(bootstrap.SaveReplacement("machine_old", false, time.Now()))
	credentials, err := config.DefaultCredentialDir()
	must(err)
	profiles := config.ProfileStore{Path: credentials, Secrets: config.FileSecretStore{Dir: filepath.Join(credentials, "secrets")}}
	const issuer = "https://api.example.test"
	must(profiles.SavePeerNetworkRelays(issuer, "account", "endpoint", []byte("{}")))
	must(profiles.Save(config.Profile{Version: 1, Issuer: issuer, CLIClientSessionID: "session"}, config.Credential{AccessToken: "a", RefreshToken: "r"}))
	policy, err := userpaths.Cache("paperboat/update-policy")
	must(err)
	must(saveCLIUpdatePolicy(filepath.Join(policy, "0000000000000000000000000000000000000000000000000000000000000000.json"), api.UpdatePolicy{Schema: "paperboat.client-update-policy/v1"}))
	root, err := helperconfig.DefaultStateRoot(os.Getenv)
	must(err)
	must(os.MkdirAll(root, 0o700))
	database, err := store.Open(t.Context(), store.Config{Root: root})
	must(err)
	must(database.Close())
	recorder, err := diagnostics.NewRecorder(diagnostics.DiskConfig{Directory: filepath.Join(filepath.Dir(root), "diagnostics"), OwnerUID: os.Geteuid(), Clock: time.Now})
	must(err)
	must(recorder.Close())

	// Every other directory Paperboat made holds a file and a link of the user.
	foreign := []string{}
	for _, directory := range []string{filepath.Dir(configPath), credentials, root, filepath.Dir(policy)} {
		file, link := filepath.Join(directory, "mine.key"), filepath.Join(directory, "mine-link")
		must(os.WriteFile(file, []byte("kept"), 0o600))
		must(os.Symlink(home, link))
		foreign = append(foreign, file, link)
	}
	command := &cobra.Command{}
	command.Flags().String("config", "", "")
	command.Flags().String("state-root", "", "")
	remove := func() {
		t.Helper()
		paths, err := userPaperboatRemovalPaths(command)
		must(err)
		for _, path := range paths {
			must(removePathPreserving(path, nil))
		}
	}
	remove()
	kept := map[string]bool{}
	for _, path := range foreign {
		kept[path] = true
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("the user's %s is gone: %v", path, err)
		}
	}
	must(filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && !kept[path] {
			t.Errorf("Paperboat's %s survived", path)
		}
		return err
	}))
	for _, path := range foreign {
		must(os.Remove(path))
	}
	remove()
	for _, directory := range []string{filepath.Dir(configPath), root, filepath.Dir(policy)} {
		if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived: %v", directory, err)
		}
	}
}
