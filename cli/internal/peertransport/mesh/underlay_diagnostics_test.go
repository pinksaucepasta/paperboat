package mesh

import (
	"bytes"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

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
