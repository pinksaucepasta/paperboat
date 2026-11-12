package localdaemon

import (
	"slices"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// Steps the daemon runs at start or in the background that report into the daemon's step tracker
// (DaemonConfig.Steps). Each is retried with backoff until it succeeds; while one is failing, the snapshot
// names it so pb status and pb doctor show it. Prior art: tailscale's health.Tracker Warnables
// (health/health.go), which tailscale status prints (cmd/tailscale/cli/status.go).
const (
	StepRegisterNetwork = "register_network"
	StepPeerAuthority   = "peer_authority"
	StepMachineServices = "machine_services"
)

var stepTitles = map[string]string{
	StepRegisterNetwork: "Registering this computer with the Paperboat network",
	StepPeerAuthority:   "Verifying the peer network configuration",
	StepMachineServices: "Reading the services your machines announce for the browser",
}

const stepRecovery = "Paperboat retries this on its own. If it keeps failing, check this computer's network and that the Paperboat server is reachable, then run `pb doctor` again."

// stepRootCauses are the health items every step depends on: while one is shown, a step failing because of it
// is not shown again (tailscale Warnable.DependsOn). It also keeps control_plane_unavailable the only item
// during a server outage, which the updater's gate relies on (UpdateProbe.ControlPlaneUnavailableOnly).
var stepRootCauses = []string{"control_plane_unavailable", NetworkDownCode, "authentication_required"}

// applySteps adds one error health item per failing step, named and with its cause. A healthy daemon adds none.
func applySteps(snapshot *localapi.Snapshot, steps *health.Registry) {
	if steps == nil {
		return
	}
	for _, item := range snapshot.Health {
		if slices.Contains(stepRootCauses, item.Code) {
			return
		}
	}
	capabilities := steps.Snapshot().Capabilities
	names := make([]string, 0, len(capabilities))
	for name, state := range capabilities {
		if state.State != health.Ready {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		state := capabilities[name]
		title := stepTitles[name]
		if title == "" {
			title = "Daemon step " + name
		}
		title += " failed: " + state.Reason
		if state.RetryAfterMs > 0 {
			title += "; retrying in " + (time.Duration(state.RetryAfterMs) * time.Millisecond).Round(time.Second).String()
		}
		if len(title) > 512 {
			title = strings.ToValidUTF8(title[:509], "") + "..."
		}
		snapshot.Health = append(snapshot.Health, localapi.HealthItem{Code: name, ETag: name, Severity: "error", Title: title, Recovery: stepRecovery})
	}
}
