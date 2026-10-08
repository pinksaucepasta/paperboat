package api

import (
	"context"
	"net/http"
	"time"
)

type MachineServicePort struct {
	Port          int    `json:"port"`
	AddressFamily string `json:"address_family"`
	BrowserURL    string `json:"browser_url,omitempty"`
}
type MachineServicesPolicy struct {
	Generation                int64                `json:"generation"`
	AutomaticDiscoveryEnabled bool                 `json:"automatic_discovery_enabled"`
	ExplicitPorts             []MachineServicePort `json:"explicit_ports"`
}
type MachineServicesSnapshot struct {
	Schema                 string               `json:"schema"`
	InstallationGeneration uint64               `json:"installation_generation"`
	BootID                 string               `json:"boot_id"`
	StartedAt              time.Time            `json:"started_at"`
	Generation             uint64               `json:"generation"`
	Services               []MachineServicePort `json:"services"`
}

// MachineServices contains only ports authorized for the authenticated account.
func (c *Client) MachineServices(ctx context.Context) ([]MachineServicesMachine, error) {
	var response struct {
		Items []MachineServicesMachine `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/machine-services", nil, &response)
	return response.Items, err
}

type MachineServicesMachine struct {
	InstallationGeneration int64                `json:"installation_generation"`
	MachineID              string               `json:"machine_id"`
	Alias                  string               `json:"alias"`
	Services               []MachineServicePort `json:"services"`
	ExpiresAt              time.Time            `json:"expires_at"`
}
