package api

import (
	"context"
	"net/http"
	"time"
)

type DeviceServicePort struct {
	Port          int    `json:"port"`
	AddressFamily string `json:"address_family"`
	BrowserURL    string `json:"browser_url,omitempty"`
}
type DeviceServicesPolicy struct {
	Generation                int64               `json:"generation"`
	AutomaticDiscoveryEnabled bool                `json:"automatic_discovery_enabled"`
	ExplicitPorts             []DeviceServicePort `json:"explicit_ports"`
}
type DeviceServicesSnapshot struct {
	Schema                 string              `json:"schema"`
	InstallationGeneration uint64              `json:"installation_generation"`
	BootID                 string              `json:"boot_id"`
	StartedAt              time.Time           `json:"started_at"`
	Generation             uint64              `json:"generation"`
	Services               []DeviceServicePort `json:"services"`
}

// DeviceServices contains only ports authorized for the authenticated account.
func (c *Client) DeviceServices(ctx context.Context) ([]DeviceServicesDevice, error) {
	var response struct {
		Items []DeviceServicesDevice `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/device-services", nil, &response)
	return response.Items, err
}

type DeviceServicesDevice struct {
	InstallationGeneration int64               `json:"installation_generation"`
	MachineID              string              `json:"machine_id"`
	Alias                  string              `json:"alias"`
	Services               []DeviceServicePort `json:"services"`
	ExpiresAt              time.Time           `json:"expires_at"`
}
