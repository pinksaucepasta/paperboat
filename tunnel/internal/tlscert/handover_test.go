package tlscert

import (
	"context"
	"testing"
	"time"
)

func activeRegistry(t *testing.T, now time.Time, hosts ...string) *Registry {
	t.Helper()
	registry, err := NewRegistry(Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i, host := range hosts {
		binding := testBinding(host, 1)
		binding.DomainID = "domain_" + host
		if err := registry.Stage(context.Background(), binding, testBundle(t, host, now, uint64(i+1))); err != nil {
			t.Fatal(err)
		}
		if err := registry.MarkReady(context.Background(), binding); err != nil {
			t.Fatal(err)
		}
		if err := registry.Activate(context.Background(), binding); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

// A replacement process with the inherited epoch serves what the serving process served, with no
// new certificate action from the server.
func TestHandedOverCertificatesAreServedByTheReplacement(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	records, err := activeRegistry(t, now, "a.example.test", "b.example.test").ExportEncoded()
	if err != nil || len(records) != 2 {
		t.Fatalf("export = %d records, %v", len(records), err)
	}
	replacement, err := NewRegistry(Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	report := replacement.ImportEncoded(context.Background(), "edge_1", "epoch_0001", records)
	if report.Imported != 2 || len(report.Dropped) != 0 {
		t.Fatalf("report = %+v", report)
	}
	for _, host := range []string{"a.example.test", "b.example.test"} {
		if certificate, err := replacement.GetCertificate(tlsClientHello(host)); err != nil || certificate == nil {
			t.Fatalf("replacement does not serve %s: %v", host, err)
		}
	}
	for _, record := range records {
		for _, b := range record {
			if b != 0 {
				t.Fatal("an imported record was not wiped")
			}
		}
	}
}

// The import goes through the registry's own checks: another process's entry and an expired one
// are dropped with a counted reason, and the good one still imports.
func TestHandedOverCertificatesFailingTheChecksAreDroppedAndCounted(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	records, err := activeRegistry(t, now, "a.example.test").ExportEncoded()
	if err != nil {
		t.Fatal(err)
	}
	mismatched, err := activeRegistry(t, now, "b.example.test").ExportEncoded()
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(48 * time.Hour) // past the certificates' 24 h lifetime
	replacement, err := NewRegistry(Config{Now: func() time.Time { return later }})
	if err != nil {
		t.Fatal(err)
	}
	if report := replacement.ImportEncoded(context.Background(), "edge_1", "epoch_0001", append([][]byte{}, records...)); report.Imported != 0 || report.Dropped["expired"] != 1 {
		t.Fatalf("expired entry: %+v", report)
	}
	replacement, err = NewRegistry(Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	report := replacement.ImportEncoded(context.Background(), "edge_2", "epoch_0001", mismatched)
	if report.Imported != 0 || report.Dropped["other_process"] != 1 {
		t.Fatalf("mismatched node: %+v", report)
	}
	if report := replacement.ImportEncoded(context.Background(), "edge_1", "epoch_0002", [][]byte{[]byte("not json")}); report.Dropped["malformed"] != 1 {
		t.Fatalf("malformed record: %+v", report)
	}
	if _, err := replacement.GetCertificate(tlsClientHello("a.example.test")); err == nil {
		t.Fatal("a dropped entry is served")
	}
}
