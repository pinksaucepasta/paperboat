package errorreport

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
)

// Dev device builds stamp DefaultSentryEnvironment=dev and report through the same reporter as releases; an
// explicit PB_SENTRY_ENVIRONMENT still wins and an unstamped build stays production.
func TestBuildStampedEnvironmentTagsReports(t *testing.T) {
	original := buildinfo.DefaultSentryEnvironment
	t.Cleanup(func() { buildinfo.DefaultSentryEnvironment = original })
	for _, tc := range []struct{ stamped, variable, want string }{
		{"", "", "production"},
		{"dev", "", "dev"},
		{"dev", "staging", "staging"},
		{"dev", "bad value!", "unknown"},
	} {
		buildinfo.DefaultSentryEnvironment = tc.stamped
		t.Setenv("PB_SENTRY_ENVIRONMENT", tc.variable)
		if got := environment(); got != tc.want {
			t.Errorf("stamped %q, variable %q: environment %q, want %q", tc.stamped, tc.variable, got, tc.want)
		}
	}
}
