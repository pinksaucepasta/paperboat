package mesh

import (
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=network-status-health
	"tailscale.com/health"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=network-status-health
	"tailscale.com/tsconst"
)

// NetworkDown reports Tailscale's NetworkStatusWarnable (health/warnings.go): no network interface has been up
// for its TimeToVisible (5 s). wgengine feeds it from netmon (SetAnyInterfaceUp).
func (s *Server) NetworkDown() bool {
	if s == nil || s.lb == nil {
		return false
	}
	tracker, ok := s.lb.sys.HealthTracker.GetOK()
	if !ok {
		return false
	}
	_, down := tracker.CurrentState().Warnings[health.WarnableCode(tsconst.HealthWarnableNetworkStatus)]
	return down
}
