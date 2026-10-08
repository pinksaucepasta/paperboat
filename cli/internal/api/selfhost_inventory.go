package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

var ErrSelfhostInventoryInvalid = errors.New("paperboat-server returned invalid self-hosted inventory")

type selfhostInventoryFailure struct{ reason string }

func (e *selfhostInventoryFailure) Error() string {
	return ErrSelfhostInventoryInvalid.Error() + ": " + e.reason
}
func (e *selfhostInventoryFailure) Unwrap() error  { return ErrSelfhostInventoryInvalid }
func invalidSelfhostInventory(reason string) error { return &selfhostInventoryFailure{reason: reason} }

type SelfhostInstallation struct {
	ScopeKind       string `json:"scope_kind"`
	ScopeID         string `json:"scope_id"`
	EnrollmentState string `json:"enrollment_state"`
	Selected        bool   `json:"-"`
	InstallationID  string `json:"installation_id"`
	NodeID          string `json:"node_id"`
	Name            string `json:"name"`
	Capability      string `json:"capability"`
	Ready           bool   `json:"ready"`
}

type SelfhostPool struct {
	Mode            string   `json:"mode"`
	InstallationIDs []string `json:"installation_ids"`
}

type SelfhostCounts struct {
	Ready    int `json:"ready"`
	Enrolled int `json:"enrolled"`
	Pending  int `json:"pending"`
}

type SelfhostInstallationPage struct {
	Installations []SelfhostInstallation `json:"installations"`
	Pagination    Pagination             `json:"pagination"`
	Counts        SelfhostCounts         `json:"counts"`
}

func (c *Client) SelfhostInstallationsPage(ctx context.Context, limit, offset int, filters url.Values) (SelfhostInstallationPage, error) {
	if limit < 1 || limit > 200 || offset < 0 || int64(offset) > 2147483647 {
		return SelfhostInstallationPage{}, errors.New("invalid self-hosted inventory page")
	}
	query := url.Values{}
	for key, values := range filters {
		query[key] = append([]string(nil), values...)
	}
	query.Set("limit", strconv.Itoa(limit))
	query.Set("offset", strconv.Itoa(offset))
	var wire struct {
		Installations *[]SelfhostInstallation `json:"installations"`
		Pagination    *struct {
			Limit      *int            `json:"limit"`
			Offset     *int            `json:"offset"`
			Total      *int            `json:"total"`
			NextOffset json.RawMessage `json:"next_offset"`
		} `json:"pagination"`
		Counts *struct {
			Ready    *int `json:"ready"`
			Enrolled *int `json:"enrolled"`
			Pending  *int `json:"pending"`
		} `json:"counts"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/selfhost/installations?"+query.Encode(), nil, &wire); err != nil {
		return SelfhostInstallationPage{}, err
	}
	if wire.Installations == nil || wire.Pagination == nil || wire.Counts == nil || wire.Pagination.Limit == nil || wire.Pagination.Offset == nil || wire.Pagination.Total == nil || len(wire.Pagination.NextOffset) == 0 || wire.Counts.Ready == nil || wire.Counts.Enrolled == nil || wire.Counts.Pending == nil {
		return SelfhostInstallationPage{}, invalidSelfhostInventory("self-hosted inventory is missing pagination or counts")
	}
	var next *int
	if err := json.Unmarshal(wire.Pagination.NextOffset, &next); err != nil {
		return SelfhostInstallationPage{}, invalidSelfhostInventory("invalid self-hosted inventory continuation")
	}
	page := SelfhostInstallationPage{Installations: *wire.Installations, Pagination: Pagination{Limit: *wire.Pagination.Limit, Offset: *wire.Pagination.Offset, Total: *wire.Pagination.Total, NextOffset: next}, Counts: SelfhostCounts{Ready: *wire.Counts.Ready, Enrolled: *wire.Counts.Enrolled, Pending: *wire.Counts.Pending}}
	p, counts := page.Pagination, page.Counts
	if p.Limit != limit || p.Offset != offset || p.Total < 0 || p.Total > 16384 || len(page.Installations) > limit || counts.Ready < 0 || counts.Enrolled < 0 || counts.Pending < 0 || counts.Ready > counts.Enrolled || counts.Enrolled > p.Total || counts.Pending != p.Total-counts.Enrolled {
		return SelfhostInstallationPage{}, invalidSelfhostInventory("invalid self-hosted inventory pagination or counts")
	}
	end := offset + len(page.Installations)
	if end > p.Total && len(page.Installations) > 0 || (p.NextOffset == nil && end < p.Total) || (p.NextOffset != nil && (len(page.Installations) == 0 || *p.NextOffset != end || end >= p.Total)) {
		return SelfhostInstallationPage{}, invalidSelfhostInventory("self-hosted inventory pagination did not advance consistently")
	}
	seenInstallations := map[string]bool{}
	for _, item := range page.Installations {
		if !validSelfhostInstallation(item) || seenInstallations[item.InstallationID] {
			return SelfhostInstallationPage{}, invalidSelfhostInventory("paperboat-server returned invalid self-hosted installation")
		}
		seenInstallations[item.InstallationID] = true
	}
	return page, nil
}

func validSelfhostInstallation(item SelfhostInstallation) bool {
	return item.InstallationID != "" && item.NodeID != "" && item.Name != "" && item.ScopeID != "" && (item.ScopeKind == "account" || item.ScopeKind == "team" || item.ScopeKind == "global") && (item.EnrollmentState == "pending" || item.EnrollmentState == "enrolled") && !(item.EnrollmentState == "pending" && item.Ready) && (item.Capability == "relay" || item.Capability == "tunnel")
}

// Pool-policy consumers must discover every authorized installation, rather than
// applying human display filters or treating one page as the complete pool.
func (c *Client) SelfhostInventory(ctx context.Context, capability string) ([]SelfhostInstallation, SelfhostPool, error) {
	if capability != "relay" && capability != "tunnel" {
		return nil, SelfhostPool{}, errors.New("invalid self-hosted capability")
	}
	installations := []SelfhostInstallation{}
	seenInstallations := map[string]bool{}
	for offset := 0; ; {
		page, err := c.SelfhostInstallationsPage(ctx, 200, offset, nil)
		if err != nil {
			return nil, SelfhostPool{}, err
		}
		for _, item := range page.Installations {
			if seenInstallations[item.InstallationID] {
				return nil, SelfhostPool{}, invalidSelfhostInventory("self-hosted inventory repeated an installation")
			}
			seenInstallations[item.InstallationID] = true
			installations = append(installations, item)
		}
		if page.Pagination.NextOffset == nil {
			break
		}
		offset = *page.Pagination.NextOffset
	}
	var pool SelfhostPool
	if err := c.do(ctx, http.MethodGet, "/v1/selfhost/pools/"+capability, nil, &pool); err != nil {
		return nil, SelfhostPool{}, err
	}
	if len(pool.InstallationIDs) > 32 || (pool.Mode != "mixed" && pool.Mode != "self-hosted-only") {
		return nil, SelfhostPool{}, invalidSelfhostInventory("paperboat-server returned invalid self-hosted pool")
	}
	selected := map[string]bool{}
	for _, id := range pool.InstallationIDs {
		if id == "" || selected[id] {
			return nil, SelfhostPool{}, invalidSelfhostInventory("paperboat-server returned invalid self-hosted pool")
		}
		selected[id] = true
	}
	items := []SelfhostInstallation{}
	for _, item := range installations {
		if item.Capability == capability && item.EnrollmentState == "enrolled" {
			item.Selected = selected[item.InstallationID] && item.ScopeKind != "global"
			items = append(items, item)
		}
	}
	return items, pool, nil
}
