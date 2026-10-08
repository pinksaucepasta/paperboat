package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type VaultLayerCoordinate struct {
	WorkspaceID string `json:"workspace_id"`
	OwnerKind   string `json:"owner_kind"`
	OwnerID     string `json:"owner_id"`
	MachineID   string `json:"machine_id"`
}
type VaultLayerSource struct {
	VaultLayerCoordinate
	KeyEpoch   uint64 `json:"key_epoch"`
	Revision   uint64 `json:"revision"`
	DocumentID string `json:"document_id"`
}
type VaultLayerRecipient struct {
	RecipientAccount       string `json:"recipient_account"`
	MachineID              string `json:"machine_id"`
	InstallationGeneration uint64 `json:"installation_generation"`
	HostKeyGeneration      uint64 `json:"host_key_generation"`
	HostPublic             string `json:"host_public"`
	DeliveryGeneration     uint64 `json:"delivery_generation"`
	DocumentID             string `json:"document_id"`
	FenceGeneration        uint64 `json:"fence_generation"`
}
type VaultLayerDelivery struct {
	Observation   *VaultLayerObservation `json:"observation"`
	Applied       bool                   `json:"applied"`
	Recipient     VaultLayerRecipient    `json:"recipient"`
	Source        VaultLayerSource       `json:"source"`
	WriterAccount string                 `json:"writer_account"`
	WriterPublic  string                 `json:"writer_public"`
	Envelope      string                 `json:"envelope"`
	State         string                 `json:"state"`
}
type VaultLayerPut struct {
	OperationID string `json:"operation_id"`
	Envelope    string `json:"envelope"`
}
type VaultLayerContext struct {
	Recipient      VaultLayerRecipient  `json:"recipient"`
	WorkspaceID    string               `json:"workspace_id"`
	ActorAccountID string               `json:"actor_account_id"`
	MachineID      string               `json:"machine_id"`
	Layers         []VaultLayerDelivery `json:"layers"`
}

func (c *Client) VaultLayerRecipients(ctx context.Context, source VaultLayerCoordinate) ([]VaultLayerRecipient, error) {
	return c.vaultLayerRecipients(ctx, source, "pending")
}
func (c *Client) VaultAllLayerRecipients(ctx context.Context, source VaultLayerCoordinate) ([]VaultLayerRecipient, error) {
	return c.vaultLayerRecipients(ctx, source, "")
}
func (c *Client) vaultLayerRecipients(ctx context.Context, source VaultLayerCoordinate, state string) ([]VaultLayerRecipient, error) {
	scoped := *c
	if err := scoped.SetWorkspace(source.WorkspaceID); err != nil {
		return nil, err
	}
	items := []VaultLayerRecipient{}
	seen := map[string]bool{}
	for offset := 0; ; {
		query := url.Values{"owner_kind": {source.OwnerKind}, "owner_id": {source.OwnerID}, "machine_id": {source.MachineID}, "limit": {"200"}, "offset": {strconv.Itoa(offset)}}
		if state != "" {
			query.Set("state", state)
		}
		var page struct {
			Items      []VaultLayerRecipient `json:"items"`
			Pagination Pagination            `json:"pagination"`
		}
		if err := scoped.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/layers?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Items {
			if r.MachineID == "" || seen[r.MachineID] {
				return nil, errors.New("duplicate ENV layer recipient")
			}
			seen[r.MachineID] = true
		}
		items = append(items, page.Items...)
		if len(page.Items) > 200 || page.Pagination.Limit != 200 || page.Pagination.Offset != offset || page.Pagination.Total < offset+len(page.Items) {
			return nil, errors.New("invalid ENV layer recipient page")
		}
		if page.Pagination.NextOffset == nil {
			if page.Pagination.Total != offset+len(page.Items) {
				return nil, errors.New("incomplete ENV layer recipient inventory")
			}
			return items, nil
		}
		next := *page.Pagination.NextOffset
		if len(page.Items) == 0 || next != offset+len(page.Items) || next >= page.Pagination.Total {
			return nil, errors.New("ENV layer pagination did not advance")
		}
		offset = next
	}
}
func (c *Client) PutVaultLayer(ctx context.Context, workspace, machine string, in VaultLayerPut) (VaultLayerDelivery, error) {
	scoped := *c
	if err := scoped.SetWorkspace(workspace); err != nil {
		return VaultLayerDelivery{}, err
	}
	var out VaultLayerDelivery
	err := scoped.vaultDataRequest(ctx, http.MethodPut, "/v1/environment/layers/"+url.PathEscape(machine), in, &out)
	return out, err
}

func (c *Client) GetVaultLayerContext(ctx context.Context, machine string) (VaultLayerContext, error) {
	var out VaultLayerContext
	err := c.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/layers/"+url.PathEscape(machine), nil, &out)
	if err == nil && (out.MachineID != machine || out.WorkspaceID != c.Workspace() || len(out.Layers) > 3) {
		return VaultLayerContext{}, errors.New("invalid ENV layer context")
	}
	return out, err
}

type VaultLayerObservation struct {
	WorkspaceID        string    `json:"workspace_id"`
	OwnerKind          string    `json:"owner_kind"`
	OwnerID            string    `json:"owner_id"`
	SourceMachineID    string    `json:"source_machine_id"`
	DeliveryGeneration uint64    `json:"delivery_generation"`
	DocumentID         string    `json:"document_id"`
	FenceGeneration    uint64    `json:"fence_generation"`
	HostRecipientKeyID string    `json:"host_recipient_key_id"`
	ObservationSeq     uint64    `json:"observation_seq"`
	State              string    `json:"state"`
	ErrorCode          *string   `json:"error_code"`
	ObservedAt         time.Time `json:"observed_at"`
}
