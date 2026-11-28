package autoupdate

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Smoke 1127 AU3/AU3M: the window that ended without an install left no trace in the updater's log.
func TestDueLogsAWindowRecordedWithoutInstalling(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	p := Preferences{Schema: PreferencesSchema, Enabled: true, LocalTime: "20:58"}
	path := filepath.Join(t.TempDir(), "maintenance.json")
	woke := time.Date(2026, 11, 27, 20, 58, 33, 0, time.UTC)
	if due, err := p.Due(path, woke, MaintenanceState{}, "abc"); err != nil || due {
		t.Fatalf("due=%v err=%v", due, err)
	}
	line := out.String()
	for _, want := range []string{"this window does not install", "none yet", "first_record=true"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
	out.Reset()
	state, err := LoadMaintenanceState(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Due(path, woke.Add(time.Minute), state, "abc"); err != nil || out.Len() != 0 {
		t.Fatalf("a window not yet due logged %q (%v)", out.String(), err)
	}
}
