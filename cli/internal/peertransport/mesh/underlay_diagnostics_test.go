package mesh

import (
	"bytes"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

type untrustedUnderlayValue struct{}

func (untrustedUnderlayValue) String() string { panic("untrusted logger value evaluated") }

func TestUnderlayDiagnosticsKeepOnlyAuthoredPrimitiveMetadata(t *testing.T) {
	if err := diagnosticlog.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "metadata.jsonl")
	output, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(output, nil)))
	defer slog.SetDefault(previous)
	d := underlayDiagnostics{engineLabel: "underlay_fixture"}
	format := "paperboat underlay: direction=%s type=%d bytes=%d endpoint=%v failed=%t transport=%s"
	endpoint := netip.MustParseAddrPort("127.0.0.1:1234")
	d.record(format, "egress", uint32(1), 148, endpoint, false, "udp")
	d.record("PRIVATE", untrustedUnderlayValue{})
	d.record(format, "egress", uint32(1), 148, untrustedUnderlayValue{}, false, "udp")
	d.record(format, "egress", uint32(1), 148, endpoint, false, "PRIVATE")
	if err := diagnosticlog.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte("native underlay packet")) != 1 || bytes.Contains(data, []byte("PRIVATE")) || bytes.Contains(data, []byte(endpoint.String())) {
		t.Fatal("underlay metadata missing or private/untrusted values retained")
	}
}

// A session sends hundreds of packets a second. Each kind of packet on one path is reported at most once a second,
// with the count of the packets it stands for; the disk ring never sees one event per packet.
func TestUnderlayDiagnosticsReportEachKindOncePerIntervalWithItsCount(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	type report struct{ packets, bytes any }
	var reports []report
	d := &underlayDiagnostics{engineLabel: "underlay_fixture", now: func() time.Time { return clock }, emit: func(_ string, args ...any) {
		fields := map[string]any{}
		for i := 0; i+1 < len(args); i += 2 {
			fields[args[i].(string)] = args[i+1]
		}
		reports = append(reports, report{fields["packets"], fields["bytes"]})
	}}
	format := "paperboat underlay: direction=%s type=%d bytes=%d endpoint=%v failed=%t transport=%s"
	endpoint := netip.MustParseAddrPort("127.0.0.1:1234")
	for i := 0; i < 1000; i++ {
		d.record(format, "egress", uint32(4), 100, endpoint, false, "udp")
		clock = clock.Add(100 * time.Microsecond)
	}
	if len(reports) != 1 || reports[0].packets != uint64(1) {
		t.Fatalf("1000 packets in 100 ms gave %d reports (%+v), want the first packet only", len(reports), reports)
	}
	d.record(format, "ingress", uint32(4), 100, endpoint, false, "udp") // another kind reports at once
	clock = clock.Add(time.Second)
	d.record(format, "egress", uint32(4), 100, endpoint, false, "udp")
	if len(reports) != 3 || reports[2].packets != uint64(1000) || reports[2].bytes != uint64(100000) {
		t.Fatalf("reports %+v, want the 1000 packets and 100000 bytes since the first report", reports)
	}
}
