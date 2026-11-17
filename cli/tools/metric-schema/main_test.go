package main

import (
	"io"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/metrics/schema"
)

// The committed docs/metrics.json is current and the real handler exposes exactly it.
func TestCommittedSchemaMatchesDescriptorsAndHandler(t *testing.T) {
	if err := schema.Run([]string{"../../docs/metrics.json"}, io.Discard, buildMetrics, verifyHandler); err != nil {
		t.Fatal(err)
	}
}
