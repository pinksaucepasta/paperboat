// pb-bootstrap is the pinned first-install verifier. It downloads no executable
// until the current release has been selected through Paperboat's TUF root.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
)

type result struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

func run(ctx context.Context, args []string) (result, error) {
	flags := flag.NewFlagSet("pb-bootstrap", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repositoryURL := flags.String("tuf-url", "https://get.pprbt.dev/tuf", "Paperboat TUF repository URL")
	stateDir := flags.String("state-dir", "", "private absolute staging directory")
	repository := flags.String("github-repository", "pinksaucepasta/paperboat-cli", "expected GitHub repository")
	version := flags.String("version", "latest", "required release version or latest")
	if err := flags.Parse(args); err != nil {
		return result{}, err
	}
	if flags.NArg() != 0 || *stateDir == "" || !filepath.IsAbs(*stateDir) || filepath.Clean(*stateDir) != *stateDir || *repository == "" || *version == "" {
		return result{}, fmt.Errorf("invalid bootstrap arguments")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	now := time.Now().UTC()
	index, err := bootstrap.FetchVerifiedReleaseIndex(ctx, *repositoryURL, filepath.Join(*stateDir, "index"), nil, now)
	if err != nil {
		return result{}, fmt.Errorf("verify signed release: %w", err)
	}
	if *version != "latest" && *version != index.Version {
		return result{}, fmt.Errorf("requested release is not the signed current release")
	}
	if !index.AvailableForNewInstall(now) {
		return result{}, fmt.Errorf("signed release is not available for new installation")
	}
	target, ok := index.Component("pb")
	if !ok || target.Repository != *repository {
		return result{}, fmt.Errorf("signed release repository does not match the expected repository")
	}
	path, err := bootstrap.FetchVerifiedReleaseComponent(ctx, *repositoryURL, filepath.Join(*stateDir, "product"), index, "pb", nil, now)
	if err != nil {
		return result{}, fmt.Errorf("download and verify signed release: %w", err)
	}
	return result{Path: path, Version: index.Version}, nil
}

func main() {
	value, err := run(context.Background(), os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "pb-bootstrap:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, "pb-bootstrap: write result:", err)
		os.Exit(1)
	}
}
