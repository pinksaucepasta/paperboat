package api

import (
	"context"
	"net/http"
	"net/url"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type VaultHostSelection struct {
	OwnerKind string `json:"owner_kind"`
	OwnerID   string `json:"owner_id"`
	Name      string `json:"name"`
}
type VaultHostState struct {
	Bundle    environmente2ee.ProjectionBundle `json:"bundle"`
	Selection []VaultHostSelection             `json:"selection"`
}
type VaultHostProvision struct {
	OperationID                 string               `json:"operation_id"`
	ExpectedSelectionGeneration uint64               `json:"expected_selection_generation"`
	Selection                   []VaultHostSelection `json:"selection"`
	Envelope                    string               `json:"envelope"`
}

func (c *Client) GetVaultHost(ctx context.Context, machine string) (VaultHostState, error) {
	var out VaultHostState
	err := c.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/hosts/"+url.PathEscape(machine), nil, &out)
	return out, err
}
func (c *Client) ProvisionVaultHost(ctx context.Context, machine string, in VaultHostProvision) (environmente2ee.ProjectionBundle, error) {
	var out environmente2ee.ProjectionBundle
	err := c.vaultDataRequest(ctx, http.MethodPut, "/v1/environment/hosts/"+url.PathEscape(machine), in, &out)
	return out, err
}
