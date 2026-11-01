package peerquic

import (
	"bytes"
	"context"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/quic-go/quic-go/qlog"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestWireDiagnosticsRetainRecoveryMetadataWithoutPrivateFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := diagnosticlog.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	previous := slog.Default()
	defer slog.SetDefault(previous)
	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	trace := newPTOTrace()
	recorder := trace.AddProducer()
	header := qlog.PacketHeader{PacketType: qlog.PacketTypeInitial, PacketNumber: 17}
	recorder.RecordEvent(qlog.PacketSent{Header: header})
	recorder.RecordEvent(&qlog.PacketReceived{Header: header})
	recorder.RecordEvent(qlog.PacketLost{Header: header})
	recorder.RecordEvent(&qlog.PTOCountUpdated{PTOCount: 2})
	recorder.RecordEvent(qlog.PacketSent{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT}, Frames: []qlog.Frame{{Frame: &qlog.ConnectionCloseFrame{ReasonPhrase: "PRIVATE_CLOSE_REASON"}}}})
	recorder.RecordEvent(qlog.ConnectionClosed{Reason: "PRIVATE_CLOSE_REASON"})
	if err := diagnosticlog.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"event":"sent"`, `"event":"received"`, `"event":"lost"`, `"pto_count":2`, `"packet_number":17`, `"close_frame":true`, `"event":"closed"`} {
		if !strings.Contains(output.String(), required) {
			t.Fatalf("missing recovery metadata %s", required)
		}
	}
	if strings.Contains(output.String(), "PRIVATE_CLOSE_REASON") {
		t.Fatal("private frame/reason was logged")
	}
	if trace.total.Load() != 2 {
		t.Fatal("diagnostics changed PTO counting")
	}
}
