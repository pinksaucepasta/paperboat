//go:build windows

package main

import (
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestWindowsLocalDaemonLaunchUsesOwnedServiceConfiguration(t *testing.T) {
	cfg := &config.Config{ServerURL: "https://api.example.test"}
	executable, configPath, serverURL, err := localDaemonLaunchValues(cfg)
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if executable != current || configPath != "" || serverURL != "" {
		t.Fatalf("SCM launch must use the installed owner configuration: executable=%q config=%q server=%q", executable, configPath, serverURL)
	}
	if cfg.ServerURL != "https://api.example.test" {
		t.Fatal("service launch changed the caller's API server")
	}
}
