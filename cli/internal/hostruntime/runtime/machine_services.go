package runtime

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/machineservices"
	"reflect"
	"sort"
)

func (s *runtimeObservationSender) machineServicesObservation(ctx context.Context) *api.MachineServicesSnapshot {
	if s.installationGeneration == 0 || s.lazyBootID == "" || s.lazyStartedAt.IsZero() {
		return nil
	}
	s.machineServicesMu.Lock()
	defer s.machineServicesMu.Unlock()
	policy := s.machineServicesPolicy
	ports := make([]api.MachineServicePort, 0)
	discover := s.machineServicesDiscover
	if discover == nil {
		discover = machineservices.Snapshot
	}
	observed, err := discover(ctx)
	// Discovery failure withdraws automatic availability rather than retaining stale ports.
	if err == nil {
		seen := map[int]bool{}
		for _, service := range observed {
			family := "ipv4"
			if service.Loopback == "::1" {
				family = "ipv6"
			}
			port := api.MachineServicePort{Port: int(service.Port), AddressFamily: family}
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

	// Explicit publications authorize their exact loopback target independently of
	// process UID. Probe only missing declared services, never arbitrary ports.
	observedSet := make(map[machineservices.Service]bool, len(observed))
	if err == nil {
		for _, service := range observed {
			observedSet[service] = true
		}
	}
	missing := make([]machineservices.Service, 0, len(policy.ExplicitPorts))
	for _, port := range policy.ExplicitPorts {
		loopback := "127.0.0.1"
		if port.AddressFamily == "ipv6" {
			loopback = "::1"
		}
		service := machineservices.Service{Port: uint16(port.Port), Loopback: loopback}
		if !observedSet[service] {
			missing = append(missing, service)
		}
	}
	explicit, probeErr := machineservices.ProbeExplicit(ctx, missing)
	if probeErr == nil {
		seen := make(map[int]bool, len(ports))
		for _, port := range ports {
			seen[port.Port] = true
		}
		for _, service := range explicit {
			if seen[int(service.Port)] {
				continue
			}
			family := "ipv4"
			if service.Loopback == "::1" {
				family = "ipv6"
			}
			ports = append(ports, api.MachineServicePort{Port: int(service.Port), AddressFamily: family})
			seen[int(service.Port)] = true
		}
	}
	if ctx.Err() != nil {
		ports = ports[:0]
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
	// The authoritative protocol bounds a snapshot to 64 ports. Deterministic
	// truncation keeps the announcement bounded; explicit probes have their own bound.
	if len(ports) > 64 {
		ports = ports[:64]
	}
	if s.machineServicesGeneration == 0 || !reflect.DeepEqual(ports, s.machineServicesPorts) {
		s.machineServicesGeneration++
		s.machineServicesPorts = ports
	}
	return &api.MachineServicesSnapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: s.installationGeneration, BootID: s.lazyBootID, StartedAt: s.lazyStartedAt.UTC(), Generation: s.machineServicesGeneration, Services: append([]api.MachineServicePort{}, ports...)}
}
func (s *runtimeObservationSender) applyMachineServicesPolicy(body []byte) {
	var response struct {
		Data struct {
			Policy api.MachineServicesPolicy `json:"machine_services_policy"`
		} `json:"data"`
	}
	decodeErr := json.Unmarshal(body, &response)
	policy := response.Data.Policy
	if decodeErr != nil || policy.Generation < 1 || len(policy.ExplicitPorts) > machineservices.MaximumExplicitServices {
		policy = api.MachineServicesPolicy{}
	}
	for _, port := range policy.ExplicitPorts {
		if port.Port < 1 || port.Port > 65535 || (port.AddressFamily != "ipv4" && port.AddressFamily != "ipv6") {
			policy = api.MachineServicesPolicy{}
			break
		}
	}
	s.machineServicesMu.Lock()
	s.machineServicesPolicy = policy
	s.machineServicesMu.Unlock()
}
