package api

import (
	"context"
	"net/http"
	"net/url"
)

// Port name scopes: one machine's port, that port on every machine of the workspace, or <name>.<domain>.
const (
	PortNameScopeMachine     = "machine"
	PortNameScopeAllMachines = "all_machines"
	PortNameScopeTopLevel    = "top_level"
)

type PortName struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// FrontDesk is where an unmatched <name>.<machine>.<domain> goes (default port 80 over http).
type FrontDesk struct {
	Port   int    `json:"port"`
	Scheme string `json:"scheme"`
}

type PortNameRecord struct {
	ID           string `json:"id"`
	Scope        string `json:"scope"`
	Name         string `json:"name"`
	Port         int    `json:"port"`
	MachineID    string `json:"machine_id,omitempty"`
	MachineAlias string `json:"machine_alias,omitempty"`
	Shadowed     bool   `json:"shadowed"`
}

type PortNameSet struct {
	Scope     string `json:"scope"`
	Name      string `json:"name"`
	Port      int    `json:"port"`
	MachineID string `json:"machine_id,omitempty"`
}

// ListPortNames lists the active workspace's machine, all-machines and top-level names.
func (c *Client) ListPortNames(ctx context.Context) ([]PortNameRecord, error) {
	var response struct {
		Items []PortNameRecord `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/port-names", nil, &response)
	return response.Items, err
}

func (c *Client) SetPortName(ctx context.Context, in PortNameSet) (out PortNameRecord, err error) {
	err = c.do(ctx, http.MethodPost, "/v1/port-names", in, &out)
	return out, err
}

func (c *Client) DeletePortName(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/port-names/"+url.PathEscape(id), nil, nil)
}

func (c *Client) GetFrontDesk(ctx context.Context, machineID string) (out FrontDesk, err error) {
	err = c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(machineID)+"/front-desk", nil, &out)
	return out, err
}

func (c *Client) SetFrontDesk(ctx context.Context, machineID string, in FrontDesk) (out FrontDesk, err error) {
	err = c.do(ctx, http.MethodPut, "/v1/machines/"+url.PathEscape(machineID)+"/front-desk", in, &out)
	return out, err
}
