package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// Desktop machine operations use the authenticated client and fixed API paths.
func (c *Client) MachineUpdateStatus(ctx context.Context, id string) (out map[string]any, err error) {
	err = c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id)+"/update-status", nil, &out)
	return
}

type MachineUpdateSummaryPage struct {
	Items      []map[string]any  `json:"items"`
	Counts     map[string]uint64 `json:"counts"`
	Pagination Pagination        `json:"pagination"`
}

var ErrFleetInventoryInvalid = errors.New("paperboat-server returned invalid fleet update inventory")

func validFleetUpdateState(state string) bool {
	switch state {
	case "not_reporting", "idle", "checking", "downloading", "staged", "activating", "deferred", "healthy", "failed", "rolled_back":
		return true
	}
	return false
}

func (c *Client) MachineUpdateSummaryPage(ctx context.Context, limit, offset int, q, state string) (MachineUpdateSummaryPage, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 || offset < 0 || int64(offset) > 2147483647 {
		return MachineUpdateSummaryPage{}, ErrFleetInventoryInvalid
	}
	values := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	if q != "" {
		values.Set("q", q)
	}
	if state != "" {
		values.Set("state", state)
	}
	var wire struct {
		Items      *[]map[string]any  `json:"items"`
		Counts     *map[string]uint64 `json:"counts"`
		Pagination *struct {
			Limit      *int            `json:"limit"`
			Offset     *int            `json:"offset"`
			Total      *int            `json:"total"`
			NextOffset json.RawMessage `json:"next_offset"`
		} `json:"pagination"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/machines/update-summary?"+values.Encode(), nil, &wire); err != nil {
		return MachineUpdateSummaryPage{}, err
	}
	if wire.Items == nil || wire.Counts == nil || wire.Pagination == nil || wire.Pagination.Limit == nil || wire.Pagination.Offset == nil || wire.Pagination.Total == nil || len(wire.Pagination.NextOffset) == 0 {
		return MachineUpdateSummaryPage{}, ErrFleetInventoryInvalid
	}
	var next *int
	if json.Unmarshal(wire.Pagination.NextOffset, &next) != nil {
		return MachineUpdateSummaryPage{}, ErrFleetInventoryInvalid
	}
	page := MachineUpdateSummaryPage{Items: *wire.Items, Counts: *wire.Counts, Pagination: Pagination{Limit: *wire.Pagination.Limit, Offset: *wire.Pagination.Offset, Total: *wire.Pagination.Total, NextOffset: next}}
	p := page.Pagination
	if p.Limit != limit || p.Offset != offset || p.Total < 0 || int64(p.Total) > 2147483647 || len(page.Items) > limit {
		return MachineUpdateSummaryPage{}, ErrFleetInventoryInvalid
	}
	end := offset + len(page.Items)
	if end > p.Total && len(page.Items) > 0 || p.NextOffset == nil && end < p.Total || p.NextOffset != nil && (len(page.Items) == 0 || *p.NextOffset != end || end >= p.Total) {
		return MachineUpdateSummaryPage{}, ErrFleetInventoryInvalid
	}
	var count uint64
	for key, value := range page.Counts {
		if !validFleetUpdateState(key) || value > uint64(p.Total)-count {
			return MachineUpdateSummaryPage{}, ErrFleetInventoryInvalid
		}
		count += value
	}
	if count != uint64(p.Total) {
		return MachineUpdateSummaryPage{}, ErrFleetInventoryInvalid
	}
	seen := map[string]bool{}
	for _, item := range page.Items {
		id, idOK := item["machine_id"].(string)
		state, stateOK := item["state"].(string)
		if !idOK || id == "" || seen[id] || !stateOK || !validFleetUpdateState(state) {
			return MachineUpdateSummaryPage{}, ErrFleetInventoryInvalid
		}
		seen[id] = true
	}
	return page, nil
}

// Desktop overview consumes the complete inventory. A failed or inconsistent
// continuation never becomes a provisional success containing only some rows.
func (c *Client) MachineUpdateSummary(ctx context.Context) (map[string]any, error) {
	items := []map[string]any{}
	seen := map[string]bool{}
	total := -1
	for offset := 0; ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := c.MachineUpdateSummaryPage(ctx, 200, offset, "", "")
		if err != nil {
			return nil, err
		}
		if total < 0 {
			total = page.Pagination.Total
		} else if total != page.Pagination.Total {
			return nil, ErrFleetInventoryInvalid
		}
		for _, item := range page.Items {
			id := item["machine_id"].(string)
			if seen[id] {
				return nil, ErrFleetInventoryInvalid
			}
			seen[id] = true
			items = append(items, item)
		}
		if page.Pagination.NextOffset == nil {
			if len(items) != total {
				return nil, ErrFleetInventoryInvalid
			}
			return map[string]any{"items": items, "counts": page.Counts}, nil
		}
		offset = *page.Pagination.NextOffset
	}
}

func (c *Client) SetMachineMetadata(ctx context.Context, id, alias, description string) (out UserMachine, err error) {
	err = c.do(ctx, http.MethodPatch, "/v1/machines/"+url.PathEscape(id), map[string]string{"alias": alias, "description": description}, &out)
	return
}

func (c *Client) RequestMachineMaintenance(ctx context.Context, id, operation, action, version, reason string) (out map[string]any, err error) {
	err = c.doWithHeaders(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/maintenance-approvals", map[string]string{"action": action, "target_version": version, "reason": reason}, &out, http.Header{"Idempotency-Key": {operation}})
	return
}

func (c *Client) MachineMaintenanceApprovals(ctx context.Context, id string, limit, offset int, q, state string) (out map[string]any, err error) {
	if limit == 0 {
		limit = 50
	}
	values := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	if q != "" {
		values.Set("q", q)
	}
	if state != "" {
		values.Set("state", state)
	}
	err = c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id)+"/maintenance-approvals?"+values.Encode(), nil, &out)
	return
}

func (c *Client) DecideMachineMaintenance(ctx context.Context, id, approval, decision string) (out map[string]any, err error) {
	if decision != "approve" && decision != "reject" {
		return nil, errors.New("select approve or reject")
	}
	err = c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/maintenance-approvals/"+url.PathEscape(approval)+"/"+decision, nil, &out)
	return
}

func (c *Client) ManagementSessions(ctx context.Context, offset int) (out map[string]any, err error) {
	if offset < 0 {
		return nil, errors.New("invalid session page")
	}
	err = c.do(ctx, http.MethodGet, "/v1/auth/cli-client-sessions?state=active&limit=50&offset="+strconv.Itoa(offset), nil, &out)
	return
}

func (c *Client) RevokeManagementSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/auth/cli-client-sessions/"+url.PathEscape(id), nil, nil)
}
