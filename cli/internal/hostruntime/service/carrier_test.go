package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func testGeneration(kind string) string {
	if kind == HostdKind || kind == CarrierKind {
		return strings.Repeat("a", 32)
	}
	return ""
}

// pbvm4 2026-11-04: replacing the one carrier unit refused new SSH for ~40 s.
// Each generation now has its own unit, so the new one serves before the old
// one retires, and a retired generation that exits 0 stays down.
func TestCarrierGenerationNativeDefinition(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			generation := testGeneration(CarrierKind)
			cfg := Config{Platform: platform, Kind: CarrierKind, Instance: "u501", Generation: generation, ConfigRoot: t.TempDir(), Executable: executable(t), User: "alice", Group: "staff", Arguments: []string{"daemon", "__runtime-carrier", "--generation", generation}, UpgradeMode: UpgradeReload, Controller: &controller{}}
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
			if platform == "linux" && (!strings.Contains(string(body), "RuntimeDirectoryMode=0700") || !strings.Contains(string(body), "RuntimeDirectory=paperboat-carrier-u501 paperboat-hostd-u501") || !strings.Contains(string(body), "RuntimeDirectoryPreserve=yes") || !strings.Contains(string(body), "Type=notify") || !strings.Contains(string(body), "WatchdogSec=30s") || !strings.Contains(string(body), "Restart=on-failure")) {
				t.Fatal("carrier private endpoint, native readiness or retirement restart policy lost")
			}
			if platform == "darwin" && !strings.Contains(string(body), "<key>SuccessfulExit</key>") {
				t.Fatal("a retired carrier generation would be relaunched by launchd")
			}
			next := cfg
			next.Generation = strings.Repeat("b", 32)
			next.Arguments = []string{"daemon", "__runtime-carrier", "--generation", next.Generation}
			second, err := New(next)
			if err != nil {
				t.Fatal(err)
			}
			if second.DefinitionPath() == first.DefinitionPath() || !strings.Contains(second.DefinitionPath(), next.Generation) {
				t.Fatal("two carrier generations share one native unit")
			}
			for _, bad := range []Config{
				func() Config { c := cfg; c.Arguments = []string{"daemon", "__runtime-hostd"}; return c }(),
				func() Config { c := cfg; c.Arguments = next.Arguments; return c }(),
				func() Config {
					c := cfg
					c.Generation = ""
					c.Arguments = []string{"daemon", "__runtime-carrier"}
					return c
				}(),
			} {
				if _, err := New(bad); !errors.Is(err, ErrInvalidDefinition) {
					t.Fatalf("invalid carrier definition admitted: %v", bad.Arguments)
				}
			}
		})
	}
}

type commandRecorder struct{ calls []string }

func (r *commandRecorder) Run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return nil
}

func TestQuitCarrierSignalsOnlyTheRetiredGenerationsMainProcess(t *testing.T) {
	generation := testGeneration(CarrierKind)
	for platform, want := range map[string]string{
		"linux":  "systemctl kill --kill-whom=main --signal=SIGQUIT paperboat-carrier-u501-" + generation + ".service",
		"darwin": "launchctl kill SIGQUIT system/" + CarrierLabel + ".u501." + generation,
	} {
		runner := &commandRecorder{}
		controller, err := CarrierController(platform, "u501", generation, 501, runner)
		if err != nil {
			t.Fatal(err)
		}
		if err := QuitCarrier(context.Background(), controller); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(runner.calls, []string{want}) {
			t.Fatalf("%s retire calls=%q want %q", platform, runner.calls, want)
		}
	}
	if _, err := CarrierController("linux", "u501", "", 501, &commandRecorder{}); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatal("carrier controller without a generation", err)
	}
}
