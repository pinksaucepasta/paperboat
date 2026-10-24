package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/helper/internal/containers"
	"github.com/pinksaucepasta/paperboat/helper/internal/reporting"
)

func TestReportChildExitLogsBoundedLastLineWithoutOtherOutput(t *testing.T) {
	reference := reporting.Reference()
	ctx := reporting.WithSupportReference(context.Background(), reference)
	exit := &containers.ChildExit{Err: errors.New("runtime container exited; supervisor will retry"), LastLine: "error: permission denied\x1b[2J"}
	var out bytes.Buffer
	reportChildExit(&out, ctx, "selfhost_child", exit)
	line := out.String()
	if !strings.Contains(line, "selfhost_child_failed") || !strings.Contains(line, reference) || !strings.Contains(line, `"error: permission denied\x1b[2J"`) {
		t.Fatalf("child exit line missing reference or last output: %q", line)
	}
	out.Reset()
	reportChildExit(&out, ctx, "selfhost_command", exit)
	reportChildExit(&out, ctx, "selfhost_child", errors.New("unrelated"))
	if out.Len() != 0 {
		t.Fatalf("unrelated failures logged a child line: %q", out.String())
	}
}
