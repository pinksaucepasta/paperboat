package runtime

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/deviceservices"
	"reflect"
	"sort"
)

func (s *runtimeObservationSender) deviceServicesObservation(ctx context.Context) *api.DeviceServicesSnapshot {
	if s.setupMode != "host" || s.installationGeneration == 0 || s.lazyBootID == "" || s.lazyStartedAt.IsZero() {
		return nil
	}
	s.deviceServicesMu.Lock()
	defer s.deviceServicesMu.Unlock()
	policy := s.deviceServicesPolicy
	ports := make([]api.DeviceServicePort, 0)
	discover := s.deviceServicesDiscover
	if discover == nil {
		discover = deviceservices.Snapshot
	}
	observed, err := discover(ctx)
	// Failure withdraws availability rather than continuing to advertise stale ports.
	if err == nil {
		seen := map[int]bool{}
		for _, service := range observed {
			family := "ipv4"
			if service.Loopback == "::1" {
				family = "ipv6"
			}
			port := api.DeviceServicePort{Port: int(service.Port), AddressFamily: family}
			allowed := policy.AutomaticDiscoveryEnabled && port.Port >= 1024
			for _, explicit := range policy.ExplicitPorts {
				if explicit.Port == port.Port && explicit.AddressFamily == family {
					allowed = true
				}
			}
			if !allowed || seen[port.Port] {
				continue
			}
			seen[port.Port] = true
			ports = append(ports, port)
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
	// The authoritative protocol bounds a snapshot to 64 ports. Deterministic
	// truncation keeps the announcement bounded without probing any target.
	if len(ports) > 64 {
		ports = ports[:64]
	}
	if s.deviceServicesGeneration == 0 || !reflect.DeepEqual(ports, s.deviceServicesPorts) {
		s.deviceServicesGeneration++
		s.deviceServicesPorts = ports
	}
	return &api.DeviceServicesSnapshot{Schema: "paperboat.device-services/v1", InstallationGeneration: s.installationGeneration, BootID: s.lazyBootID, StartedAt: s.lazyStartedAt.UTC(), Generation: s.deviceServicesGeneration, Services: append([]api.DeviceServicePort{}, ports...)}
}
func (s *runtimeObservationSender) applyDeviceServicesPolicy(body []byte) {
	var response struct {
		Data struct {
			Policy api.DeviceServicesPolicy `json:"device_services_policy"`
		} `json:"data"`
	}
	decodeErr := json.Unmarshal(body, &response)
	policy := response.Data.Policy
	if decodeErr != nil || policy.Generation < 1 || len(policy.ExplicitPorts) > 64 {
		policy = api.DeviceServicesPolicy{}
	}
	for _, port := range policy.ExplicitPorts {
		if port.Port < 1 || port.Port > 65535 || (port.AddressFamily != "ipv4" && port.AddressFamily != "ipv6") {
			policy = api.DeviceServicesPolicy{}
			break
		}
	}
	s.deviceServicesMu.Lock()
	s.deviceServicesPolicy = policy
	s.deviceServicesMu.Unlock()
}
