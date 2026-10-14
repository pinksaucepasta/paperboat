package operatorreporting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProjectReferenceAcceptsProviderNumberOrObject(t *testing.T) {
	for _, raw := range []string{`42`, `"42"`, `{"id":"42","secret":"must-not-return"}`} {
		var p projectReference
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.ID != "42" {
			t.Fatalf("%s => %+v %v", raw, p, err)
		}
	}
}
func TestSafeMonitorRejectsCrossProjectAndDropsMaliciousFields(t *testing.T) {
	project := sentryProject{ID: "42", Slug: "paperboat-relay"}
	var raw sentryMonitor
	if err := json.Unmarshal([]byte(`{"slug":"pb-control","name":"Control health","status":"active","type":"cron_job","isMuted":false,"project":{"id":"42","slug":"go"},"environments":[{"name":"production","status":"error","lastCheckIn":"2026-09-19T16:46:11Z","nextCheckIn":"2026-09-19T16:47:00Z","nextCheckInLatest":"2026-09-19T16:48:00Z","activeIncident":{"startingTimestamp":"2026-09-19T16:46:00Z","resolvingTimestamp":null,"brokenNotice":"private"}},{"name":"validation","status":"active"}],"config":{"schedule":[1,"minute"],"checkin_margin":1,"max_runtime":1,"timezone":"UTC","failure_issue_threshold":1,"recovery_threshold":1,"schedule_type":"interval","alert_rule_id":777294},"owner":null,"query":"token=secret"}`), &raw); err != nil {
		t.Fatal(err)
	}
	out, ok := safeMonitor(raw, project)
	if !ok || !out.Enabled || out.Slug != "pb-control" || out.CheckInMargin != 1 || out.MaxRuntime != 1 || out.FailureThreshold != 1 || out.RecoveryThreshold != 1 || len(out.Environments) != 2 || out.Environments[0].Name != "production" || out.Environments[0].Status != "error" || out.Environments[0].NextLatest.IsZero() || out.Environments[0].Incident == nil || out.Environments[0].Incident.StartedAt.IsZero() || out.Environments[1].Name != "validation" || out.Environments[1].Status != "active" {
		t.Fatalf("safe monitor=%+v ok=%v", out, ok)
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "query") {
		t.Fatalf("leaked provider fields: %s", encoded)
	}
	raw.Project.ID = "43"
	if _, ok = safeMonitor(raw, project); ok {
		t.Fatal("cross-project monitor accepted")
	}
}
func TestProviderGETUsesBoundedTLSRequestAndRejectsOversize(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/0/organizations/paperboat/monitors/" || r.URL.Query().Get("project") != "42" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request=%s headers=%v", r.URL, r.Header)
		}
		w.Header().Set("Link", `<https://sentry.invalid/?cursor=next>; rel="next"; results="true"`)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	service := &Service{baseURL: u, org: "paperboat", token: "token", client: server.Client()}
	header, err := service.get(context.Background(), "/api/0/organizations/paperboat/monitors/", url.Values{"project": {"42"}}, new([]sentryMonitor))
	if err != nil || nextCursor(header.Get("Link")) != "next" {
		t.Fatalf("get err=%v header=%v", err, header)
	}
	large := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
	}))
	defer large.Close()
	u, _ = url.Parse(large.URL)
	service.baseURL = u
	service.client = large.Client()
	if _, err = service.get(context.Background(), "/api/0/test", nil, new(any)); err != ErrUnavailable {
		t.Fatalf("oversize=%v", err)
	}
}
func TestMonitorBoundsAndCursorValidation(t *testing.T) {
	if safeCount(-1) != 0 || safeCount(86400*366) != 0 || safeCount(60) != 60 {
		t.Fatal("unsafe provider duration accepted")
	}
	if validCursor("bad\nvalue") || !validCursor("0:100:0") {
		t.Fatal("cursor validation")
	}
	if got := safeDisplay("owner@example.com"); got != "" {
		t.Fatalf("unsafe display=%q", got)
	}
}

func TestCheckInUsesExecutionChronologyNotIngestionTime(t *testing.T) {
	var raw sentryCheckIn
	if err := json.Unmarshal([]byte(`{"id":"3fd52eee-0ac6-43f4-885e-f422ff3f147e","environment":"sentry-validation","status":"error","duration":108,"dateCreated":"2026-09-19T16:46:42.343798Z","dateAdded":"2026-09-19T16:46:11Z","dateUpdated":"2026-09-19T16:46:11Z","dateClock":"2026-09-19T16:46:00Z","dateInProgress":"2026-09-19T16:46:11Z","expectedTime":null,"monitorConfig":{"ownerEmail":"private@example.com"}}`), &raw); err != nil {
		t.Fatal(err)
	}
	out, ok := safeCheckIn(raw)
	if !ok || out.StartedAt.Format(time.RFC3339) != "2026-09-19T16:46:11Z" || out.FinishedAt.Format(time.RFC3339) != "2026-09-19T16:46:11Z" || out.DurationMS != 108 {
		t.Fatalf("check-in=%+v ok=%v", out, ok)
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "dateCreated") {
		t.Fatalf("unsafe chronology payload=%s", encoded)
	}
}

func TestSafeMonitorRejectsUnknownEnumsInvalidConfigAndEnvironmentOverflow(t *testing.T) {
	project := sentryProject{ID: "42", Slug: "server"}
	base := sentryMonitor{Slug: "health", Status: "active", Type: "cron_job", Project: projectReference{ID: "42"}}
	base.Config.MaxRuntime, base.Config.FailureIssueThreshold, base.Config.RecoveryThreshold = 1, 1, 1
	if _, ok := safeMonitor(base, project); !ok {
		t.Fatal("valid monitor rejected")
	}
	unknown := base
	unknown.Status = "customer-secret"
	if _, ok := safeMonitor(unknown, project); ok {
		t.Fatal("unknown status accepted")
	}
	invalidConfig := base
	invalidConfig.Config.MaxRuntime = 0
	if _, ok := safeMonitor(invalidConfig, project); ok {
		t.Fatal("invalid config accepted")
	}
	overflow := base
	overflow.Environments = make([]sentryMonitorEnvironment, 17)
	if _, ok := safeMonitor(overflow, project); ok {
		t.Fatal("environment overflow silently truncated")
	}
	badTime := base
	badTime.LastCheckIn = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, ok := safeMonitor(badTime, project); ok {
		t.Fatal("unbounded timestamp accepted")
	}
}

func TestSafeMonitorAcceptsLiveWireWithOmittedType(t *testing.T) {
	var raw sentryMonitor
	if err := json.Unmarshal([]byte(`{"slug":"pb-validation-availability","name":"Availability","status":"active","project":{"id":"42"},"environments":[],"config":{"checkin_margin":1,"max_runtime":1,"failure_issue_threshold":1,"recovery_threshold":1}}`), &raw); err != nil {
		t.Fatal(err)
	}
	got, ok := safeMonitor(raw, sentryProject{ID: "42", Slug: "server"})
	if !ok || got.Type != "" || !got.Enabled {
		t.Fatalf("monitor=%+v ok=%v", got, ok)
	}
	raw.Type = "provider-secret"
	if _, ok := safeMonitor(raw, sentryProject{ID: "42", Slug: "server"}); ok {
		t.Fatal("unknown nonempty monitor type accepted")
	}
}

func TestSafeDetectorProjectsLatestIssueAndConditionComparisons(t *testing.T) {
	var raw sentryDetector
	if err := json.Unmarshal([]byte(`{"id":"detector-1","name":"pb-validation-server-latency","projectId":"42","isEnabled":true,"latestGroup":{"id":"148104376","status":"resolved"},"conditionGroup":{"conditions":[{"type":"gt","comparison":1,"conditionResult":75},{"type":"lte","comparison":0,"conditionResult":0}]}}`), &raw); err != nil {
		t.Fatal(err)
	}
	got, ok := safeDetector(raw, sentryProject{ID: "42", Slug: "server"})
	if !ok || got.IssueID != "148104376" || got.Status != "resolved" || got.FailureThreshold != 1 || got.RecoveryThreshold != 0 {
		t.Fatalf("detector=%+v ok=%v", got, ok)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "conditionResult") || strings.Contains(string(encoded), "75") {
		t.Fatalf("provider condition detail leaked: %s", encoded)
	}
}
