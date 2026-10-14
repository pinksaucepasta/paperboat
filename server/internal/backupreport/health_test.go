package backupreport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestInternalHealthIsBoundedAndContainsNoEvidenceIdentity(t *testing.T) {
	var absent *Service
	if h, err := absent.Health(t.Context()); err != nil || h.Backup.State != HealthUnavailable || h.Restore.State != HealthUnavailable {
		t.Fatal(h)
	}
	s, _ := NewService(nil, Config{})
	if h, err := s.Health(t.Context()); err != nil || h.Backup.State != HealthNotConfigured || h.Restore.State != HealthNotConfigured || h.Backup.AgeSeconds != nil {
		t.Fatal(h)
	}
	s, _ = NewService(nil, Config{Token: strings.Repeat("x", 32)})
	if h, err := s.Health(t.Context()); err != nil || h.Backup.State != HealthUnavailable {
		t.Fatal(h)
	}
	now := time.Now()
	r := validFixture(now)
	r.DatabaseID = "PRIVATE_DATABASE"
	r.RunID = "PRIVATE_RUN"
	health := recordHealth(&Record{Report: r}, now, time.Hour)
	b, _ := json.Marshal(health)
	if strings.Contains(string(b), "PRIVATE") || health.State != HealthCurrent || health.AgeSeconds == nil || *health.AgeSeconds != 1 {
		t.Fatalf("health %s", b)
	}
	r.CompletedAt = now.Add(time.Second)
	if h := recordHealth(&Record{Report: r}, now, time.Hour); *h.AgeSeconds != 0 {
		t.Fatal("negative age", h)
	}
	if recordHealth(nil, now, time.Hour).State != HealthUnknown {
		t.Fatal("missing evidence current")
	}
}
