package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

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

// VaultHostSummary contains metadata only. Selections and host key bindings are
// fetched from the exact host before encryption.
type VaultHostSummary struct {
	MachineID           string `json:"machine_id"`
	SelectionGeneration uint64 `json:"selection_generation"`
	ProjectionRevision  uint64 `json:"projection_revision"`
	FenceGeneration     uint64 `json:"fence_generation"`
	State               string `json:"state"`
}

func (c *Client) PendingVaultHosts(ctx context.Context) ([]VaultHostSummary, error) {
	const limit = 200
	items := []VaultHostSummary{}
	seen := map[string]bool{}
	for offset := 0; ; {
		var page struct {
			Items      []VaultHostSummary `json:"items"`
			Pagination Pagination         `json:"pagination"`
		}
		query := url.Values{"state": {"pending"}, "limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
		if err := c.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/hosts?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		if len(page.Items) > limit || page.Pagination.Limit != limit || page.Pagination.Offset != offset || page.Pagination.Total < offset+len(page.Items) {
			return nil, errors.New("invalid ENV host inventory page")
		}
		for _, item := range page.Items {
			if item.MachineID == "" || item.State != "pending" || item.ProjectionRevision == 0 || seen[item.MachineID] {
				return nil, errors.New("invalid ENV host inventory item")
			}
			seen[item.MachineID] = true
		}
		items = append(items, page.Items...)
		if page.Pagination.NextOffset == nil {
			return items, nil
		}
		next := *page.Pagination.NextOffset
		if len(page.Items) == 0 || next != offset+len(page.Items) || next >= page.Pagination.Total {
			return nil, errors.New("ENV host inventory pagination did not advance")
		}
		offset = next
	}
}
