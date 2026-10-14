package service

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestCarrierStableNativeDefinition(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			cfg := Config{Platform: platform, Kind: CarrierKind, Instance: "u501", ConfigRoot: t.TempDir(), Executable: executable(t), User: "alice", Group: "staff", Arguments: []string{"daemon", "__runtime-carrier"}, UpgradeMode: UpgradeReload, Controller: &controller{}}
			first, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			body, err := first.render()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "__runtime-carrier") || !strings.Contains(string(body), "alice") {
				t.Fatal("carrier command or enrolled identity omitted")
			}
			if platform == "linux" && (!strings.Contains(string(body), "RuntimeDirectoryMode=0700") || !strings.Contains(string(body), "RuntimeDirectory=paperboat-carrier-u501 paperboat-hostd-u501") || !strings.Contains(string(body), "RuntimeDirectoryPreserve=yes") || !strings.Contains(string(body), "Type=notify") || !strings.Contains(string(body), "WatchdogSec=30s")) {
				t.Fatal("carrier private endpoint or native readiness lost")
			}
			cfg.Executable = executable(t)
			next, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if next.DefinitionPath() != first.DefinitionPath() || !strings.Contains(filepath.Base(next.DefinitionPath()), "carrier") {
				t.Fatal("replacement overlaps carrier native identity")
			}
			cfg.Arguments = []string{"daemon", "__runtime-hostd"}
			if _, err := New(cfg); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatal("foreign role admitted as carrier")
			}
		})
	}
}
