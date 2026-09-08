//go:build windows

package updated

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestWindowsDeferredManualResolverSurvivesRestart(t *testing.T) {
	for _, mode := range []string{"update", "maintenance", ""} {
		t.Run(mode, func(t *testing.T) {
			journal := testWindowsActivationJournal()
			journal.ManualMode = mode
			journal.BlockedReason = autoupdate.BlockedActiveTerminalSessions
			raw, err := json.Marshal(journal)
			if err != nil {
				t.Fatal(err)
			}
			journal = windowsActivationJournal{}
			if err = json.Unmarshal(raw, &journal); err != nil {
				t.Fatal(err)
			}
			calls := ""
			resolver := func(name string, found bool) workerupdate.Resolver {
				return func(context.Context) (workerupdate.Release, bool, error) {
					calls += name
					return workerupdate.Release{Version: journal.Version}, found, nil
				}
			}
			_, found, gotMode, err := resolveWindowsQueuedRelease(context.Background(), journal, resolver("automatic", false), resolver("update", true), resolver("maintenance", true))
			want := mode
			if want == "" {
				want = "automatic"
			}
			if err != nil || calls != want || gotMode != mode || found != (mode != "") {
				t.Fatalf("mode=%q calls=%q found=%t err=%v", gotMode, calls, found, err)
			}
			if mode != "" {
				denied := func(context.Context) (workerupdate.Release, bool, error) {
					return workerupdate.Release{Version: "different"}, true, nil
				}
				if _, _, _, err = resolveWindowsQueuedRelease(context.Background(), journal, denied, denied, denied); err == nil {
					t.Fatal("replacement version inherited manual intent")
				}
			}
		})
	}
}
