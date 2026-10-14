package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalDocumentIsSortedAndBounded(t *testing.T) {
	data, err := canonicalDocument()
	if err != nil {
		t.Fatal(err)
	}
	var value document
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if value.SchemaVersion != schemaVersion || len(value.Metrics) == 0 {
		t.Fatalf("document=%+v", value)
	}
	for index, item := range value.Metrics {
		if index > 0 && value.Metrics[index-1].Name >= item.Name {
			t.Fatalf("metrics are not uniquely sorted at %q", item.Name)
		}
		for label, allowed := range item.Labels {
			if len(allowed) == 0 {
				t.Fatalf("metric %q label %q is unbounded", item.Name, label)
			}
			for valueIndex := 1; valueIndex < len(allowed); valueIndex++ {
				if allowed[valueIndex-1] >= allowed[valueIndex] {
					t.Fatalf("metric %q label %q values are not uniquely sorted", item.Name, label)
				}
			}
		}
	}
}

func TestRealMetricHandlerMatchesDescriptors(t *testing.T) {
	if err := verifyHandler(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandFailuresKeepCausesWithoutExposingInputs(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "PRIVATE_METRIC_DOCUMENT")
	err := run([]string{missingPath})
	if err == nil || err.Error() != "metric schema document could not be read" || strings.Contains(err.Error(), missingPath) {
		t.Fatalf("read failure exposed its path: %v", err)
	}
	var readFailure *os.PathError
	if !errors.As(err, &readFailure) {
		t.Fatalf("read failure lost its filesystem cause: %T %v", err, err)
	}

	writePath := t.TempDir()
	err = run([]string{"-write", writePath})
	if err == nil || err.Error() != "metric schema document could not be written" || strings.Contains(err.Error(), writePath) {
		t.Fatalf("write failure exposed its path: %v", err)
	}
	var writeFailure *os.PathError
	if !errors.As(err, &writeFailure) {
		t.Fatalf("write failure lost its filesystem cause: %T %v", err, err)
	}
}

func TestInvalidFlagValueIsNotEchoedAndKeepsUsageExit(t *testing.T) {
	const privateValue = "PRIVATE_METRIC_FLAG_VALUE"
	err := run([]string{"-write=" + privateValue, "docs/metrics.json"})
	if err == nil || err.Error() != "invalid metric-schema flags" || strings.Contains(err.Error(), privateValue) {
		t.Fatalf("flag failure exposed its input: %v", err)
	}
	var failure commandFailure
	if !errors.As(err, &failure) || failure.exitCode != 2 || failure.cause == nil || !strings.Contains(failure.cause.Error(), privateValue) {
		t.Fatal("flag failure did not preserve its original parse cause and usage exit")
	}
}
