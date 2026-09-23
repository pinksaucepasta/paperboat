package errorreport

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"os"
)

type recordingTransport struct{ events []*sentry.Event }

func (t *recordingTransport) Configure(sentry.ClientOptions)        {}
func (t *recordingTransport) SendEvent(event *sentry.Event)         { t.events = append(t.events, event) }
func (t *recordingTransport) Flush(time.Duration) bool              { return true }
func (t *recordingTransport) FlushWithContext(context.Context) bool { return true }
func (t *recordingTransport) Close()                                {}

func TestDisabledByDefaultAndIncompleteConfiguration(t *testing.T) {
	for _, key := range []string{"PB_SENTRY_ENABLED", "PB_SENTRY_DSN", "PB_SENTRY_RELEASE"} {
		t.Setenv(key, "")
	}
	if FromEnvironment().Enabled() {
		t.Fatal("reporting enabled by default")
	}
	t.Setenv("PB_SENTRY_ENABLED", "true")
	if FromEnvironment().Enabled() {
		t.Fatal("reporting enabled without DSN and release")
	}
}

func TestCaptureIsSanitizedAndRateBounded(t *testing.T) {
	transport := &recordingTransport{}
	reporter := newReporter("https://public@example.invalid/1", "paperboat-test", transport)
	ctx := supportref.WithContext(context.Background(), "pb-0123456789abcdef0123456789abcdef")
	for i := 0; i < maximumPerMinute+2; i++ {
		reporter.Capture(ctx, "pb", "unexpected_failure")
	}
	if len(transport.events) != maximumPerMinute {
		t.Fatalf("events=%d", len(transport.events))
	}
	for _, event := range transport.events {
		if event.Message != "" || len(event.Breadcrumbs) != 0 || event.Request != nil || len(event.Contexts) != 0 {
			t.Fatal("unsafe event data retained")
		}
		if event.Tags["support_reference"] == "" || len(event.Exception) != 1 || event.Exception[0].Value != "" {
			t.Fatal("safe classification/reference missing")
		}
		for _, frame := range event.Exception[0].Stacktrace.Frames {
			if filepath.IsAbs(frame.Filename) || frame.AbsPath != "" || frame.Vars != nil {
				t.Fatalf("unsafe frame: %#v", frame)
			}
		}
	}
}

func TestDistributionReportingDefaults(t *testing.T) {
	oldDSN, oldRelease := buildinfo.DefaultSentryDSN, buildinfo.DefaultSentryRelease
	t.Cleanup(func() { buildinfo.DefaultSentryDSN, buildinfo.DefaultSentryRelease = oldDSN, oldRelease })
	for _, tt := range []struct {
		name, defaultDSN, defaultRelease, enabled, dsn, release string
		want                                                    bool
		wantDSN, wantRelease                                    string
	}{
		{name: "source defaults"},
		{name: "source destination alone", dsn: "https://own@example.invalid/2", release: "custom"},
		{name: "source opt in incomplete", enabled: "true"},
		{name: "source own project", enabled: "true", dsn: "https://own@example.invalid/2", release: "custom", want: true, wantDSN: "https://own@example.invalid/2", wantRelease: "custom"},
		{name: "official defaults", defaultDSN: "https://public@example.invalid/1", defaultRelease: "paperboat:2026.09.19.0", want: true, wantDSN: "https://public@example.invalid/1", wantRelease: "paperboat:2026.09.19.0"},
		{name: "official disabled", defaultDSN: "https://public@example.invalid/1", defaultRelease: "paperboat:2026.09.19.0", enabled: "false"},
		{name: "invalid enable", defaultDSN: "https://public@example.invalid/1", defaultRelease: "paperboat:2026.09.19.0", enabled: "yes"},
		{name: "incomplete embedded", defaultDSN: "https://public@example.invalid/1"},
		{name: "official own project override", defaultDSN: "https://public@example.invalid/1", defaultRelease: "paperboat:2026.09.19.0", dsn: "https://own@example.invalid/2", release: "custom", want: true, wantDSN: "https://own@example.invalid/2", wantRelease: "custom"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			buildinfo.DefaultSentryDSN, buildinfo.DefaultSentryRelease = tt.defaultDSN, tt.defaultRelease
			t.Setenv("PB_SENTRY_ENABLED", tt.enabled)
			t.Setenv("PB_SENTRY_DSN", tt.dsn)
			t.Setenv("PB_SENTRY_RELEASE", tt.release)
			t.Setenv("PB_SENTRY_LOGS_ENABLED", "false")
			t.Setenv("PB_SENTRY_METRICS_ENABLED", "false")
			t.Setenv("PB_SENTRY_TRACES_SAMPLE_RATE", "0")
			r := FromEnvironment()
			defer r.Flush(context.Background())
			if r.Enabled() != tt.want {
				t.Fatalf("enabled=%v want=%v", r.Enabled(), tt.want)
			}
			if tt.want {
				opts := r.client.Options()
				if opts.Dsn != tt.wantDSN || opts.Release != tt.wantRelease {
					t.Fatal("wrong reporting destination or release")
				}
			}
		})
	}
}

// Invoked on a linker-stamped Linux test binary by release configuration acceptance.
func TestOfficialLinkedReportingDefaults(t *testing.T) {
	if os.Getenv("PB_TEST_OFFICIAL_DEFAULTS") != "true" {
		t.Skip("requires explicitly stamped acceptance build")
	}
	for _, key := range []string{"PB_SENTRY_ENABLED", "PB_SENTRY_DSN", "PB_SENTRY_RELEASE", "PB_SENTRY_LOGS_ENABLED", "PB_SENTRY_METRICS_ENABLED", "PB_SENTRY_TRACES_SAMPLE_RATE"} {
		t.Setenv(key, "")
	}
	if buildinfo.DefaultSentryDSN != "https://public@example.invalid/1" || buildinfo.DefaultSentryRelease != "paperboat:2026.09.19.0" {
		t.Fatal("release link defaults missing")
	}
	r := FromEnvironment()
	defer r.Flush(context.Background())
	if !r.Enabled() || !r.logs || !r.metrics || r.rate != 0.1 {
		t.Fatal("official build did not enable all configured signal defaults")
	}
	t.Setenv("PB_SENTRY_ENABLED", "false")
	disabled := FromEnvironment()
	defer disabled.Flush(context.Background())
	if disabled.Enabled() {
		t.Fatal("official build ignored explicit disable")
	}
}
