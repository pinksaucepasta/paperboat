//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSystemServiceScopeUsesUnifiedOSServices(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		units := []string{"paperboat-hostd-u1001.service", "paperboat-updated-u1001.service", "paperboat-runtime-privileged-u1001.service"}
		tool, verb, output := "/usr/bin/systemctl", "is-active", "active\n"
		if platform == "darwin" {
			units = []string{"system/com.pinksaucepasta.paperboat.hostd.u1001", "system/com.pinksaucepasta.paperboat.updated.u1001"}
			tool, verb, output = "/bin/launchctl", "print", "state = running\n"
		}
		for missing := -1; missing < len(units); missing++ {
			var calls []string
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
				if missing >= 0 && args[len(args)-1] == units[missing] {
					return nil, errors.New("inactive")
				}
				return []byte(output), nil
			}
			scope, err := systemServiceScopeWithRunner(context.Background(), platform, 1001, run)
			if scope != "system" || (err != nil) != (missing >= 0) {
				t.Fatalf("%s missing=%d scope=%s err=%v", platform, missing, scope, err)
			}
			count := len(units)
			if missing >= 0 {
				count = missing + 1
			}
			want := make([]string, count)
			for i := range want {
				want[i] = tool + " " + verb + " " + units[i]
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("%s calls=%v want=%v", platform, calls, want)
			}
		}
	}
}

func TestSystemServiceScopeRejectsLegacyOnlyAndMissingPlatformServices(t *testing.T) {
	legacyOnly := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "paperboat-runtime-host.service" {
			return []byte("active\n"), nil
		}
		return nil, errors.New("unit missing")
	}
	if _, err := systemServiceScopeWithRunner(context.Background(), "linux", 1001, legacyOnly); err == nil {
		t.Fatal("legacy runtime-host service satisfied host readiness")
	}
	if _, err := systemServiceScopeWithRunner(context.Background(), "linux", 1001, func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("unit missing")
	}); err == nil {
		t.Fatal("missing current unit satisfied host readiness")
	}
}
