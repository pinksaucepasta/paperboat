package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"sort"
	"strings"

	hosttelemetry "github.com/pinksaucepasta/paperboat/internal/hostruntime/telemetry"
)

const schemaVersion = 1

type document struct {
	SchemaVersion int      `json:"schema_version"`
	Metrics       []metric `json:"metrics"`
}

type metric struct {
	Name   string              `json:"name"`
	Kind   hosttelemetry.Kind  `json:"kind"`
	Labels map[string][]string `json:"labels,omitempty"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		_, _ = fmt.Fprintln(os.Stderr, "metric-schema:", err)
		var failure commandFailure
		if errors.As(err, &failure) && failure.exitCode > 0 {
			os.Exit(failure.exitCode)
		}
		os.Exit(1)
	}
}

type commandFailure struct {
	message  string
	cause    error
	exitCode int
}

func (e commandFailure) Error() string { return e.message }

func (e commandFailure) Unwrap() error { return e.cause }

func run(args []string) error {
	flags := flag.NewFlagSet("metric-schema", flag.ContinueOnError)
	flagOutput := &bytes.Buffer{}
	flags.SetOutput(flagOutput)
	write := flags.Bool("write", false, "write the canonical metric schema")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if _, writeErr := os.Stderr.Write(flagOutput.Bytes()); writeErr != nil {
				return commandFailure{message: "metric-schema help could not be written", cause: writeErr}
			}
			return flag.ErrHelp
		}
		return commandFailure{message: "invalid metric-schema flags", cause: err, exitCode: 2}
	}
	if flags.NArg() != 1 {
		return errors.New("usage: metric-schema [-write] DOCUMENT")
	}
	data, err := canonicalDocument()
	if err != nil {
		return commandFailure{message: "metric schema could not be generated", cause: err}
	}
	path := flags.Arg(0)
	if *write {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return commandFailure{message: "metric schema document could not be written", cause: err}
		}
		return nil
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return commandFailure{message: "metric schema document could not be read", cause: err}
	}
	if !bytes.Equal(current, data) {
		return errors.New("metric schema is stale; run make metrics-generate")
	}
	if err := verifyHandler(); err != nil {
		return commandFailure{message: "metric handler verification failed", cause: err}
	}
	return nil
}

func canonicalDocument() ([]byte, error) {
	descriptors := hosttelemetry.DefaultDescriptors()
	metrics := make([]metric, 0, len(descriptors))
	seen := make(map[string]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		if _, ok := seen[descriptor.Name]; ok {
			return nil, fmt.Errorf("duplicate metric %q", descriptor.Name)
		}
		seen[descriptor.Name] = struct{}{}
		item := metric{Name: descriptor.Name, Kind: descriptor.Kind}
		if len(descriptor.Labels) != 0 {
			item.Labels = make(map[string][]string, len(descriptor.Labels))
			for label, values := range descriptor.Labels {
				if len(values) == 0 {
					return nil, fmt.Errorf("metric %q label %q is unbounded", descriptor.Name, label)
				}
				allowed := make([]string, 0, len(values))
				for value := range values {
					allowed = append(allowed, value)
				}
				sort.Strings(allowed)
				item.Labels[label] = allowed
			}
		}
		metrics = append(metrics, item)
	}
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].Name < metrics[j].Name })
	data, err := json.MarshalIndent(document{SchemaVersion: schemaVersion, Metrics: metrics}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func verifyHandler() error {
	descriptors := hosttelemetry.DefaultDescriptors()
	registry, err := hosttelemetry.NewRegistry(descriptors)
	if err != nil {
		return err
	}
	histograms := make(map[string]struct{})
	for _, descriptor := range descriptors {
		labels := make(map[string]string, len(descriptor.Labels))
		for label, values := range descriptor.Labels {
			allowed := make([]string, 0, len(values))
			for value := range values {
				allowed = append(allowed, value)
			}
			sort.Strings(allowed)
			labels[label] = allowed[0]
		}
		if descriptor.Kind == hosttelemetry.Histogram {
			histograms[descriptor.Name] = struct{}{}
		}
		if err := registry.Record(descriptor.Name, 1, labels); err != nil {
			return fmt.Errorf("record %s: %w", descriptor.Name, err)
		}
	}
	recorder := httptest.NewRecorder()
	registry.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if recorder.Code != 200 {
		return fmt.Errorf("status %d", recorder.Code)
	}
	emitted := make(map[string]struct{})
	for _, line := range strings.Split(strings.TrimSpace(recorder.Body.String()), "\n") {
		name := strings.Fields(line)[0]
		if index := strings.IndexByte(name, '{'); index >= 0 {
			name = name[:index]
		}
		for base := range histograms {
			if name == base+"_bucket" || name == base+"_sum" || name == base+"_count" {
				name = base
				break
			}
		}
		emitted[name] = struct{}{}
	}
	for _, descriptor := range descriptors {
		if _, ok := emitted[descriptor.Name]; !ok {
			return fmt.Errorf("documented metric %q was not emitted", descriptor.Name)
		}
		delete(emitted, descriptor.Name)
	}
	if len(emitted) != 0 {
		return fmt.Errorf("handler emitted undocumented metrics: %v", emitted)
	}
	return nil
}
