package preview

import (
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// TestCarrierSourceJudgesAdmissionsOnTheServerClock: the server issues carrier admissions that live 2 minutes; a
// machine whose clock is 10 minutes ahead must still accept a fresh one (X-27: every carrier was refused as expired,
// the machine went offline and inbound calls failed for the whole skew).
func TestCarrierSourceJudgesAdmissionsOnTheServerClock(t *testing.T) {
	sent := time.Now()
	serverNow := sent.Add(-10 * time.Minute) // the server is 10 minutes behind this machine's clock
	serverclock.Observe(serverNow.UTC().Format(http.TimeFormat), sent, sent)
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	source, err := NewMachineAttachmentSessionSource(MachineAttachmentSessionSourceConfig{StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	admissionExpires := serverNow.Add(2 * time.Minute)
	if !admissionExpires.After(source.clock().UTC()) {
		t.Fatalf("a fresh 2-minute admission reads as expired: source clock %s, expiry %s", source.clock().UTC(), admissionExpires.UTC())
	}
	if serverNow.Add(-time.Second).After(source.clock().UTC()) {
		t.Fatal("an admission that expired on the server reads as valid")
	}
}
