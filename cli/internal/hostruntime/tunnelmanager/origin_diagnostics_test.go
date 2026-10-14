package tunnelmanager

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestOriginFailureDiagnosticSurvivesQuietProcessLogger(t *testing.T) {
	var output bytes.Buffer
	prior, priorDefault := originDiagnosticLogger, slog.Default()
	originDiagnosticLogger = slog.New(slog.NewTextHandler(&output, nil))
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer func() { originDiagnosticLogger = prior; slog.SetDefault(priorDefault) }()
	logOriginStreamFailure(context.Background(), "http_read", io.ErrUnexpectedEOF)
	if !strings.Contains(output.String(), "stage=http_read code=unexpected_eof") {
		t.Fatalf("missing finite failure category: %s", output.String())
	}
}
