package liveconnections

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestCountsFollowOpenAndClosedConnections(t *testing.T) {
	base := Current()
	closeSSH := TrackSSH()
	closeForward := TrackForward()
	TrackSSH()()
	if got := Current(); got.SSH != base.SSH+1 || got.Forwards != base.Forwards+1 || got.Total() != base.Total()+2 {
		t.Fatalf("counts = %+v from %+v", got, base)
	}
	closeSSH()
	closeSSH() // closing twice must not undercount
	closeForward()
	if got := Current(); got != base {
		t.Fatalf("counts = %+v, want %+v", got, base)
	}
}

type captureHandler struct{ records chan slog.Record }

func (captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records <- r
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

// An update that proceeds because the daemon could not answer leaves a record naming who asked and why it failed.
func TestRecordUnconfirmedLeavesADiagnosticRecord(t *testing.T) {
	handler := captureHandler{records: make(chan slog.Record, 4)}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	RecordUnconfirmed("updater", errors.New("probe timed out"))
	select {
	case record := <-handler.records:
		attributes := map[string]string{}
		record.Attrs(func(a slog.Attr) bool { attributes[a.Key] = a.Value.String(); return true })
		if record.Level != slog.LevelWarn || attributes["source"] != "updater" || attributes["error"] != "probe timed out" {
			t.Fatalf("record %v %v", record.Level, attributes)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no diagnostic record")
	}
}
