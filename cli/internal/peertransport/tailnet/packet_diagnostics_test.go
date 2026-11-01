package tailnet

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

func TestRejectedPacketWriteRecordsReasonWithoutPayload(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := diagnosticlog.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	previous := slog.Default()
	defer slog.SetDefault(previous)
	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	packet := &Packet{}
	packet.closed.Store(true)
	if _, err := packet.Write([]byte("PRIVATE_PAYLOAD")); !errors.Is(err, net.ErrClosed) {
		t.Fatal("closed socket accepted write")
	}
	if err := diagnosticlog.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"reason":"socket_closed"`) || strings.Contains(output.String(), "PRIVATE_PAYLOAD") {
		t.Fatal("missing rejection reason or private payload was logged")
	}
}
