package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
)

type NativePrivateGrantRequest struct {
	OperationID  string `json:"operation_id"`
	ResourceKind string `json:"resource_kind"`
	ResourceID   string `json:"resource_id"`
	RouteID      string `json:"route_id"`
	Protocol     string `json:"protocol"`
	Selector     string `json:"selector,omitempty"`
}
type NativePrivateGrant struct {
	Target struct {
		AccountID          string `json:"account_id"`
		UserID             string `json:"user_id"`
		EnvironmentID      string `json:"environment_id"`
		MachineID          string `json:"machine_id"`
		AccessSessionID    string `json:"access_session_id"`
		ResourceKind       string `json:"resource_kind"`
		ResourceID         string `json:"resource_id"`
		ResourceGeneration uint64 `json:"resource_generation"`
		RouteID            string `json:"route_id"`
		RouteGeneration    uint64 `json:"route_generation"`
		TargetGeneration   uint64 `json:"target_generation"`
		Protocol           string `json:"protocol"`
		TargetScheme       string `json:"target_scheme"`
		TargetAddress      string `json:"target_address"`
	} `json:"target"`
	Credential string    `json:"credential"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (c *Client) IssueNativePrivateGrant(ctx context.Context, request NativePrivateGrantRequest) (NativePrivateGrant, error) {
	var grant NativePrivateGrant
	if err := c.doStrict(ctx, http.MethodPost, "/v1/native-private-access/grants", request, &grant); err != nil {
		return NativePrivateGrant{}, err
	}
	t := grant.Target
	binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: t.ResourceKind, ResourceID: t.ResourceID, ResourceGeneration: t.ResourceGeneration, RouteID: t.RouteID, RouteGeneration: t.RouteGeneration, TargetGeneration: t.TargetGeneration, OwnerEndpointID: t.MachineID, Protocol: t.Protocol, TargetScheme: t.TargetScheme, TargetAddress: t.TargetAddress, ExpiresAt: grant.ExpiresAt}
	if grant.Credential == "" || len(grant.Credential) > 16<<10 || binding.Validate(time.Now().UTC()) != nil || t.AccountID == "" || t.UserID == "" || t.EnvironmentID == "" || t.AccessSessionID == "" {
		return NativePrivateGrant{}, errors.New("unsafe native private grant")
	}
	return grant, nil
}

func (g NativePrivateGrant) Binding() ([]byte, error) {
	t := g.Target
	return json.Marshal(nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: t.ResourceKind, ResourceID: t.ResourceID, ResourceGeneration: t.ResourceGeneration, RouteID: t.RouteID, RouteGeneration: t.RouteGeneration, TargetGeneration: t.TargetGeneration, OwnerEndpointID: t.MachineID, Protocol: t.Protocol, TargetScheme: t.TargetScheme, TargetAddress: t.TargetAddress, ExpiresAt: g.ExpiresAt})
}
