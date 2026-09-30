package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
)

func TestInstallSuppliesCurrentExecutableAndPropagatesFailure(t *testing.T) {
	oldSource, oldInstall := runningInstallSource, installSuppliedExecutable
	t.Cleanup(func() { runningInstallSource, installSuppliedExecutable = oldSource, oldInstall })
	source := installsource.Source{Version: "custom-test", Distribution: installsource.Custom, SHA256: "source-digest"}
	runningInstallSource = func() (string, installsource.Source, error) { return "/supplied/pb", source, nil }
	var failure error
	installSuppliedExecutable = func(ctx context.Context, path string, got installsource.Source, directory string) (string, error) {
		if path != "/supplied/pb" || got != source || directory != "/commands" {
			t.Fatal("supplied executable identity changed")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("installation has no deadline")
		}
		return "/commands/pb", failure
	}
	command := platformInstallCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetArgs([]string{"--json", "--install-dir", "/commands"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Data struct {
			Executable       string   `json:"executable"`
			AutomaticUpdates bool     `json:"automatic_update_checks"`
			Enrollment       string   `json:"enrollment"`
			BrowserHTTPS     string   `json:"browser_https"`
			Warnings         []string `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Executable != "/commands/pb" || result.Data.AutomaticUpdates || result.Data.Enrollment != "unchanged" {
		t.Fatalf("unexpected result: %s", out.String())
	}
	failure = errors.New("installation failed")
	out.Reset()
	if err := command.Execute(); !errors.Is(err, failure) {
		t.Fatal("install failure not returned", err)
	}
	if out.Len() != 0 {
		t.Fatal("failed installation reported success")
	}
	var warnings bytes.Buffer
	command.SetErr(&warnings)
	failure = &hostruntimecmd.BrowserTrustPendingError{Cause: errors.New("administrator approval unavailable"), Recovery: "sudo pb daemon device-guard trust"}
	if err := command.Execute(); err != nil {
		t.Fatalf("committed core installation was reported as failed: %v", err)
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Executable != "/commands/pb" || result.Data.BrowserHTTPS != "trust_pending" || len(result.Data.Warnings) != 1 || warnings.Len() == 0 {
		t.Fatalf("browser trust pending state was hidden: result=%s warning=%s", out.String(), warnings.String())
	}
	for _, flag := range []string{"skip-verification", "skip-download", "fresh", "source", "source-version"} {
		if command.Flags().Lookup(flag) != nil {
			t.Fatal("unexpected install bypass flag", flag)
		}
	}
}
