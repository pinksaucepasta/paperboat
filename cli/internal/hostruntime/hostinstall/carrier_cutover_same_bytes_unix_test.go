//go:build linux || darwin

package hostinstall

import (
	"context"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
)

// pb update --from of the serving carrier's own signed bytes differs only in install labels. The upgrade must not
// stage a carrier generation, which would share the serving generation's name, socket and lock and then retire it
// (smoke 1126 apple, mp5, lp12: "hostd.sock.c<gen>: no such file", previous version restored; lp10 11-27 on .21:
// the carrier unit stopped ~70 s after a "successful" reinstall). Other bytes still stage one.
func TestSameBytesFromDoesNotStageACarrierGeneration(t *testing.T) {
	previousRead, previousStage := readRetainedCarrier, stageCandidate
	t.Cleanup(func() { readRetainedCarrier, stageCandidate = previousRead, previousStage })
	signed := installsource.Source{Version: "2026.11.26.21", Platform: "linux", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Length: 7, Distribution: installsource.Official, AutomaticUpdates: true}
	readRetainedCarrier = func(context.Context, string) (CarrierConfig, error) {
		return CarrierConfig{Source: signed}, nil
	}
	var staged []bool
	stageCandidate = func(_ context.Context, _ Request, carrier bool) (StagedCandidate, error) {
		staged = append(staged, carrier)
		return StagedCandidate{}, nil
	}
	from := signed
	from.Distribution, from.AutomaticUpdates = installsource.Custom, false
	request := Request{UID: 1001, Source: from}
	upgrade, err := stageUnixUpgrade(context.Background(), request, platformPaths(request.UID), true)
	if err != nil || upgrade != nil || len(staged) != 1 || staged[0] {
		t.Fatalf("same bytes with other labels: upgrade=%v err=%v carrier staged=%v", upgrade, err, staged)
	}
	other := signed
	other.SHA256 = strings.Repeat("b", 64)
	staged = nil
	// The stub reports no generation, so the staged generation is refused after the stage: only the decision matters.
	_, _ = stageUnixUpgrade(context.Background(), Request{UID: 1001, Source: other}, platformPaths(1001), true)
	if len(staged) != 1 || !staged[0] {
		t.Fatalf("other bytes must stage a carrier generation: %v", staged)
	}
}
