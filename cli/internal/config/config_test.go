package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
)

func TestLoadUsesReleaseServerDefaultWithoutPersistingIt(t *testing.T) {
	previous := buildinfo.DefaultServerURL
	buildinfo.DefaultServerURL = "https://api.release.example"
	t.Cleanup(func() { buildinfo.DefaultServerURL = previous })

	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerURL != buildinfo.DefaultServerURL {
		t.Fatalf("server URL = %q, want release default %q", cfg.ServerURL, buildinfo.DefaultServerURL)
	}
}

func TestFavoritesEnforceSharedLimitAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := &Config{path: path}
	for index := range MaxFavorites {
		if err := cfg.SetFavorite("machine", fmt.Sprintf("m%d", index), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.SetFavorite("session", "extra", true); !errors.Is(err, ErrFavoriteLimit) {
		t.Fatalf("limit error=%v", err)
	}
	if err := cfg.SetFavorite("machine", "m0", false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetFavorite("session", "s0", true); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.IsFavorite("machine", "m0") || !reloaded.IsFavorite("session", "s0") || len(reloaded.Favorites) != MaxFavorites {
		t.Fatalf("favorites=%+v", reloaded.Favorites)
	}
}

func TestNormalizeServerURL(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		valid     bool
	}{
		{"https://api.example/", "https://api.example", true},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", true},
		{"http://localhost", "http://localhost", true},
		{"http://api.example", "", false},
		{"https://user:pass@api.example", "", false},
		{"https://api.example/path", "", false},
		{"https://api.example?token=x", "", false},
	} {
		got, err := NormalizeServerURL(tc.raw)
		if tc.valid && (err != nil || got != tc.want) {
			t.Fatalf("NormalizeServerURL(%q) = %q, %v", tc.raw, got, err)
		}
		if !tc.valid && err == nil {
			t.Fatalf("NormalizeServerURL(%q) succeeded", tc.raw)
		}
	}
}

func TestControlSyncAddressIsExplicitAndTLS(t *testing.T) {
	cfg := &Config{ControlSyncAddress: "sync.example.test:443"}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil || cfg.ControlSyncAddress != "sync.example.test:443" {
		t.Fatalf("valid sync address = %q, %v", cfg.ControlSyncAddress, err)
	}
	for _, value := range []string{"http://sync.example.test:443", "sync.example.test", "https://user@sync.example.test:443", "https://sync.example.test:443/path"} {
		bad := &Config{ControlSyncAddress: value}
		bad.applyDefaults()
		if err := bad.Validate(); err == nil {
			t.Errorf("unsafe sync address %q accepted", value)
		}
	}
}

func TestSaveUsesRestrictedPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := &Config{path: path, ServerURL: "https://api.example"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !configTestFilePrivate(path, info) {
		t.Fatalf("config file is not private: mode=%o", info.Mode().Perm())
	}
}

func TestRemovedNetworkOverridesAreNotLoadedOrSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server_url":"https://api.example","machine_suffix":"devbox","machine_loopback_cidr":"127.212.0.0/16"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "machine_suffix") || strings.Contains(string(data), "machine_loopback_cidr") {
		t.Fatalf("retired network overrides persisted: %s", data)
	}
}

func TestLocalAccessDefaultsCanonicalizesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"local_access":{"domain":" DEV.Example.COM ","service_aliases":[{"machine_alias":" HP ","name":"JellyFin","port":8989}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalAccess.Domain != "dev.example.com" || len(cfg.LocalAccess.ServiceAliases) != 1 {
		t.Fatalf("local access = %+v", cfg.LocalAccess)
	}
	want := LocalServiceAlias{MachineAlias: "hp", Name: "jellyfin", Port: 8989}
	if got := cfg.LocalAccess.ServiceAliases[0]; got != want {
		t.Fatalf("service alias = %+v, want %+v", got, want)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LocalAccess.Domain != "dev.example.com" {
		t.Fatalf("reloaded domain = %q", reloaded.LocalAccess.Domain)
	}
	if got := reloaded.LocalAccess.ServiceAliases; len(got) != 1 || got[0] != want {
		t.Fatalf("reloaded aliases = %+v, want %+v", got, want)
	}
}

func TestLocalAccessDefaultsToPaperboatDomain(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalAccess.Domain != DefaultLocalAccessDomain {
		t.Fatalf("default browser domain = %q, want %q", cfg.LocalAccess.Domain, DefaultLocalAccessDomain)
	}
}

func TestLocalAccessValidationRejectsUnsafeOrAmbiguousSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  LocalAccessConfig
	}{
		{name: "public suffix", cfg: LocalAccessConfig{Domain: "com"}},
		{name: "scheme", cfg: LocalAccessConfig{Domain: "https://example.com"}},
		{name: "wildcard", cfg: LocalAccessConfig{Domain: "*.example.com"}},
		{name: "numeric service label", cfg: LocalAccessConfig{Domain: "dev.example.com", ServiceAliases: []LocalServiceAlias{{MachineAlias: "hp", Name: "8989", Port: 8989}}}},
		{name: "invalid service label", cfg: LocalAccessConfig{Domain: "dev.example.com", ServiceAliases: []LocalServiceAlias{{MachineAlias: "hp", Name: "jellyfin.web", Port: 8989}}}},
		{name: "invalid machine label", cfg: LocalAccessConfig{Domain: "dev.example.com", ServiceAliases: []LocalServiceAlias{{MachineAlias: "hp.other", Name: "jellyfin", Port: 8989}}}},
		{name: "zero port", cfg: LocalAccessConfig{Domain: "dev.example.com", ServiceAliases: []LocalServiceAlias{{MachineAlias: "hp", Name: "jellyfin", Port: 0}}}},
		{name: "duplicate after canonicalization", cfg: LocalAccessConfig{Domain: "dev.example.com", ServiceAliases: []LocalServiceAlias{{MachineAlias: "HP", Name: "Jellyfin", Port: 8989}, {MachineAlias: "hp", Name: "jellyfin", Port: 3000}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{LocalAccess: tc.cfg}
			cfg.applyDefaults()
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid local access configuration was accepted")
			}
		})
	}
}

func TestLocalAccessAllows128AliasesAndRejects129(t *testing.T) {
	makeConfig := func(count int) *Config {
		aliases := make([]LocalServiceAlias, count)
		for index := range aliases {
			aliases[index] = LocalServiceAlias{MachineAlias: "hp", Name: fmt.Sprintf("service-%d", index), Port: 8989}
		}
		cfg := &Config{LocalAccess: LocalAccessConfig{Domain: "dev.example.com", ServiceAliases: aliases}}
		cfg.applyDefaults()
		return cfg
	}
	if err := makeConfig(MaxLocalServiceAliases).Validate(); err != nil {
		t.Fatalf("maximum alias count rejected: %v", err)
	}
	if err := makeConfig(MaxLocalServiceAliases + 1).Validate(); err == nil {
		t.Fatal("alias count above guard limit was accepted")
	}
}

func TestLoadPreservesExplicitZeroDialRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"connect":{"dial_retries":0},"server_url":"https://api.example"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Connect.DialRetries != 0 {
		t.Fatalf("dial retries = %d, want explicit zero", cfg.Connect.DialRetries)
	}
}

func TestLoadAppliesDialRetryDefaultWhenOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server_url":"https://api.example"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Connect.DialRetries != DefaultDialRetries {
		t.Fatalf("dial retries = %d, want %d", cfg.Connect.DialRetries, DefaultDialRetries)
	}
	if cfg.Observability.MaxEventLogBytes != DefaultTelemetryMaxBytes {
		t.Fatalf("telemetry max bytes = %d, want %d", cfg.Observability.MaxEventLogBytes, DefaultTelemetryMaxBytes)
	}
	if cfg.Connect.TerminalOutputQueueChunks != DefaultTerminalOutputQueueChunks ||
		cfg.Connect.TerminalOutputBatchMilliseconds != DefaultTerminalOutputBatchMilliseconds ||
		cfg.Connect.TerminalOutputBufferBytes != DefaultTerminalOutputBufferBytes {
		t.Fatalf("terminal output defaults = %+v", cfg.Connect)
	}
	if got := strings.Join(TerminalEnv, ","); got != "TERM,COLORTERM,TERM_PROGRAM,TERM_PROGRAM_VERSION,LANG,LC_ALL,LC_CTYPE" {
		t.Fatalf("terminal environment = %q", got)
	}
	if cfg.StatusBar.Mode != DefaultStatusBarMode || cfg.StatusBar.Fullscreen != DefaultStatusBarFullscreen || cfg.StatusBar.Theme != DefaultStatusBarTheme || cfg.StatusBar.NoticeSeconds != DefaultStatusBarNoticeSeconds {
		t.Fatalf("status bar defaults = %+v", cfg.StatusBar)
	}
	if got, want := strings.Join(cfg.StatusBar.Left, ","), "project,session"; got != want {
		t.Fatalf("status bar left = %q, want %q", got, want)
	}
	if got, want := strings.Join(cfg.StatusBar.Center, ","), "activity"; got != want {
		t.Fatalf("status bar center = %q, want %q", got, want)
	}
	if got, want := strings.Join(cfg.StatusBar.Right, ","), "credits,connection"; got != want {
		t.Fatalf("status bar right = %q, want %q", got, want)
	}
}

func TestLoadIgnoresRemovedPeerRaceOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server_url":"https://api.example","connect":{"peer_relay_delay_milliseconds":5000,"peer_wss_delay_milliseconds":5000,"peer_connect_timeout_milliseconds":15000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("removed peer race overrides affected config loading: %v", err)
	}
}

func TestLoadRejectsInvalidTerminalPerformanceConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, raw := range []string{
		`{"connect":{"terminal_output_batch_milliseconds":-1}}`,
		`{"connect":{"input_partial_flush_milliseconds":-1}}`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("Load accepted invalid terminal configuration: %s", raw)
		}
	}
}

func TestLoadValidatesStatusBarWidgetsAndPreservesEmptyRegions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"status_bar":{"left":[],"center":["storage"],"right":["credits","connection"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StatusBar.Left == nil || len(cfg.StatusBar.Left) != 0 {
		t.Fatalf("explicit empty left region was replaced: %#v", cfg.StatusBar.Left)
	}
	for _, raw := range []string{
		`{"status_bar":{"left":["unknown"]}}`,
		`{"status_bar":{"left":["project"],"right":["project"]}}`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("Load accepted invalid status bar config: %s", raw)
		}
	}
}

func TestLoadRejectsInvalidStatusBarMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"status_bar":{"mode":"sometimes"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted invalid status_bar.mode")
	}
}

func TestLoadValidatesStatusBarBehaviorAndColors(t *testing.T) {
	for _, raw := range []string{
		`{"status_bar":{"fullscreen":"sometimes"}}`,
		`{"status_bar":{"theme":"rainbow"}}`,
		`{"status_bar":{"colors":{"accent":"javascript"}}}`,
		`{"status_bar":{"colors":{"error":"#xyzxyz"}}}`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("Load accepted invalid config: %s", raw)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"status_bar":{"fullscreen":"show","theme":"dark","colors":{"accent":"bright_cyan","error":"#ff0033"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load rejected valid status bar config: %v", err)
	}
}

func TestSavePreservesExplicitZeroDialRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := &Config{path: path, dialRetriesConfigured: true}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Connect.DialRetries != 0 {
		t.Fatalf("dial retries = %d after save, want explicit zero", reloaded.Connect.DialRetries)
	}
}

func TestTelemetryPathDefaultsBesideConfigAndCanBeDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paperboat", "config.json")
	cfg := &Config{path: path}
	if got, want := cfg.TelemetryPath(), filepath.Join(filepath.Dir(path), "telemetry.jsonl"); got != want {
		t.Fatalf("TelemetryPath() = %q, want %q", got, want)
	}
	cfg.Observability.DisableEventLog = true
	if got := cfg.TelemetryPath(); got != "" {
		t.Fatalf("disabled TelemetryPath() = %q", got)
	}
	cfg.Observability.DisableEventLog = false
	cfg.Observability.EventLogPath = "events/custom.jsonl"
	if got, want := cfg.TelemetryPath(), filepath.Join(filepath.Dir(path), "events", "custom.jsonl"); got != want {
		t.Fatalf("relative TelemetryPath() = %q, want %q", got, want)
	}
}

func TestLocalMachineProxyValidationAndBounds(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.LocalAccess.MachineProxies = []LocalMachineProxy{{MachineAlias: " HOMELAB ", Port: 80}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.LocalAccess.MachineProxies[0].MachineAlias != "homelab" {
		t.Fatal("machine alias not normalized")
	}
	for _, proxies := range [][]LocalMachineProxy{{{MachineAlias: "bad.machine", Port: 80}}, {{MachineAlias: "hp", Port: 0}}, {{MachineAlias: "HP", Port: 80}, {MachineAlias: "hp", Port: 81}}} {
		c.LocalAccess.MachineProxies = proxies
		if err := c.Validate(); err == nil {
			t.Fatalf("invalid proxy accepted: %+v", proxies)
		}
	}
	c.LocalAccess.MachineProxies = make([]LocalMachineProxy, MaxLocalMachineProxies+1)
	if err := c.Validate(); err == nil {
		t.Fatal("proxy bound not enforced")
	}
}
