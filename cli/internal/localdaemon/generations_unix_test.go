//go:build darwin || linux

package localdaemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// generationRunner is a service manager fake that tracks which daemon generations run.
type generationRunner struct {
	running map[string]bool
	calls   []string
}

func (r *generationRunner) generation(args []string) string {
	for _, arg := range args {
		for _, part := range strings.FieldsFunc(arg, func(c rune) bool { return c == '-' || c == '.' || c == '/' }) {
			if validGeneration(part) {
				return part
			}
		}
	}
	return ""
}

func (r *generationRunner) Run(_ context.Context, name string, args ...string) error {
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	generation := r.generation(args)
	switch {
	case strings.Contains(call, "enable --now"), strings.Contains(call, " start "), strings.Contains(call, "bootstrap"), strings.Contains(call, "kickstart"):
		r.running[generation] = true
	case strings.Contains(call, "disable --now"), strings.Contains(call, " stop "), strings.Contains(call, "bootout"):
		r.running[generation] = false
	case strings.Contains(call, "is-active"), strings.HasPrefix(call, "launchctl print"):
		if !r.running[generation] {
			return errors.New("not running")
		}
	}
	return nil
}

func (r *generationRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	running := r.running[r.generation(args)]
	if name == "launchctl" {
		if running {
			return "state = running\npid = 42\nactive count = 1\n", nil
		}
		return "state = not running\n", nil
	}
	if running {
		return "LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nSubState=running\n", nil
	}
	return "LoadState=loaded\nUnitFileState=enabled\nActiveState=inactive\nSubState=dead\n", nil
}

func writeGenerationDefinition(t *testing.T, platform, home, generation string, age time.Duration) string {
	t.Helper()
	path := generationDefinitionPath(platform, home, generation)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("previous generation"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return path
}

func indexOf(calls []string, fragment string) int {
	for index, call := range calls {
		if strings.Contains(call, fragment) {
			return index
		}
	}
	return -1
}

// A user's daemon upgrade starts the new generation's unit, then asks the old one to drain with
// SIGQUIT; the old unit is never stopped, so the forwarded connections it holds keep flowing.
func TestReplaceStartsTheNewGenerationThenQuitsTheOld(t *testing.T) {
	old, next := strings.Repeat("a", 32), strings.Repeat("b", 32)
	for platform, want := range map[string][2]string{
		"linux":  {"enable --now paperboatd-" + next, "systemctl --user kill --kill-whom=main --signal=SIGQUIT paperboatd-" + old},
		"darwin": {"kickstart gui/501/com.pinksaucepasta.paperboatd." + next, "launchctl kill SIGQUIT gui/501/com.pinksaucepasta.paperboatd." + old},
	} {
		t.Run(platform, func(t *testing.T) {
			home := t.TempDir()
			oldDefinition := writeGenerationDefinition(t, platform, home, old, time.Hour)
			runner := &generationRunner{running: map[string]bool{old: true}}
			config := serviceConfig{Platform: platform, Home: home, Generation: next, Executable: serviceExecutable(t), Username: "test", Group: "staff", UID: 501, Environment: map[string]string{"HOME": home}, Runner: runner}
			restore, err := replaceService(context.Background(), config, "")
			if err != nil {
				t.Fatal(err)
			}
			started, quit := indexOf(runner.calls, want[0]), indexOf(runner.calls, want[1])
			if started < 0 || quit < 0 || quit < started {
				t.Fatalf("new generation must start before the old one is quit (start=%d quit=%d):\n%s", started, quit, strings.Join(runner.calls, "\n"))
			}
			if !runner.running[old] || !runner.running[next] {
				t.Fatalf("both generations must run during the drain: %v", runner.running)
			}
			if _, err := os.Stat(oldDefinition); err != nil {
				t.Fatalf("the draining generation lost its unit: %v", err)
			}
			// A failed readiness check removes the new unit and gives the old generation a fresh process.
			runner.calls = nil
			if err := restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(generationDefinitionPath(platform, home, next)); !os.IsNotExist(err) {
				t.Fatalf("rolled back generation remains: %v", err)
			}
			if runner.running[next] || !runner.running[old] {
				t.Fatalf("rollback must leave only the previous generation running: %v\n%s", runner.running, strings.Join(runner.calls, "\n"))
			}
		})
	}
}

// The live generation removes the units of generations that exited after their drain, never one
// that still drains or itself.
func TestPruneRemovesOnlyExitedGenerations(t *testing.T) {
	current, draining, exited := strings.Repeat("c", 32), strings.Repeat("d", 32), strings.Repeat("e", 32)
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			home := t.TempDir()
			paths := map[string]string{}
			for index, generation := range []string{exited, draining, current} {
				paths[generation] = writeGenerationDefinition(t, platform, home, generation, time.Duration(3-index)*time.Hour)
			}
			runner := &generationRunner{running: map[string]bool{current: true, draining: true}}
			config := serviceConfig{Platform: platform, Home: home, Executable: serviceExecutable(t), Username: "test", Group: "staff", UID: 501, Environment: map[string]string{"HOME": home}, Runner: runner}
			if err := pruneExitedGenerations(context.Background(), config, current); err != nil {
				t.Fatal(err)
			}
			for generation, keep := range map[string]bool{current: true, draining: true, exited: false} {
				if _, err := os.Stat(paths[generation]); (err == nil) != keep {
					t.Fatalf("generation %s kept=%v, want %v", generation[:1], err == nil, keep)
				}
			}
			if !runner.running[draining] {
				t.Fatal("pruning stopped a draining generation")
			}
		})
	}
}
