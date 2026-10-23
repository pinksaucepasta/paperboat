package doctor

import "testing"

// C020 (round 9, hp): `pb doctor` right after `pb setup` said "degraded" for a daemon that was only
// starting. A pending check makes the report "starting", not degraded; a real warning or failure
// still decides the overall.
func TestAStartingDaemonMakesTheReportStartingNotDegraded(t *testing.T) {
	pass := Check{Status: StatusPass}
	pending := Check{Status: StatusPending}
	for name, test := range map[string]struct {
		checks []Check
		want   string
	}{
		"all pass":               {[]Check{pass, pass}, "healthy"},
		"daemon starting":        {[]Check{pass, pending}, "starting"},
		"starting and a warning": {[]Check{pending, {Status: StatusWarning}}, "degraded"},
		"warning then starting":  {[]Check{{Status: StatusWarning}, pending}, "degraded"},
		"starting and a failure": {[]Check{pending, {Status: StatusFail}}, "unhealthy"},
	} {
		if got := overall(test.checks); got != test.want {
			t.Errorf("%s: overall=%q, want %q", name, got, test.want)
		}
	}
}
