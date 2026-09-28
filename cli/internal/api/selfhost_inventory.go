package api

import (
	"context"
	"errors"
	"net/http"
)

type SelfhostInstallation struct {
	InstallationID string `json:"installation_id"`
	NodeID         string `json:"node_id"`
	Name           string `json:"name"`
	Capability     string `json:"capability"`
	Ready          bool   `json:"ready"`
}

type SelfhostPool struct {
	Mode            string   `json:"mode"`
	InstallationIDs []string `json:"installation_ids"`
}

func (c *Client) SelfhostInventory(ctx context.Context, capability string) ([]SelfhostInstallation, SelfhostPool, error) {
	if capability != "relay" && capability != "tunnel" {
		return nil, SelfhostPool{}, errors.New("invalid self-hosted capability")
	}
	var installations struct {
		Items []SelfhostInstallation `json:"installations"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/selfhost/installations", nil, &installations); err != nil {
		return nil, SelfhostPool{}, err
	}
	var pool SelfhostPool
	if err := c.do(ctx, http.MethodGet, "/v1/selfhost/pools/"+capability, nil, &pool); err != nil {
		return nil, SelfhostPool{}, err
	}
	if len(installations.Items) > 256 || len(pool.InstallationIDs) > 32 || (pool.Mode != "mixed" && pool.Mode != "self-hosted-only") {
		return nil, SelfhostPool{}, errors.New("paperboat-server returned invalid self-hosted inventory")
	}
	selected := make(map[string]bool, len(pool.InstallationIDs))
	for _, id := range pool.InstallationIDs {
		if id == "" || selected[id] {
			return nil, SelfhostPool{}, errors.New("paperboat-server returned invalid self-hosted pool")
		}
		selected[id] = true
	}
	items := make([]SelfhostInstallation, 0, len(installations.Items))
	for _, item := range installations.Items {
		if item.InstallationID == "" || item.NodeID == "" || item.Name == "" || (item.Capability != "relay" && item.Capability != "tunnel") {
			return nil, SelfhostPool{}, errors.New("paperboat-server returned invalid self-hosted installation")
		}
		if item.Capability == capability && selected[item.InstallationID] {
			items = append(items, item)
		}
	}
	return items, pool, nil
}
