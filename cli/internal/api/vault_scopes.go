package api

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/vaultprotocol"
	"net/http"
	"net/url"

	"github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
)

type VaultScopeState = vaultprotocol.VaultScopeState
type VaultScopePut = vaultprotocol.VaultScopePut
type VaultSharingState = vaultprotocol.VaultSharingState
type VaultTeamMember = vaultprotocol.VaultTeamMember
type VaultTeamState = vaultprotocol.VaultTeamState
type VaultTeamCreate = vaultprotocol.VaultTeamCreate
type VaultMemberGrant = vaultprotocol.VaultMemberGrant
type VaultTeamRotate = vaultprotocol.VaultTeamRotate
type VaultGrantState = vaultprotocol.VaultGrantState
type VaultScopeDocument = vaultprotocol.VaultScopeDocument
type VaultPersonalScopeStage = vaultprotocol.VaultPersonalScopeStage
type VaultRecordReplacement = vaultprotocol.VaultRecordReplacement
type VaultPersonalReset = vaultprotocol.VaultPersonalReset
type VaultPersonalScopes = vaultprotocol.VaultPersonalScopes
type VaultPersonalRotate = vaultprotocol.VaultPersonalRotate

func DecodeVaultScope(s VaultScopeState) (environmente2ee.VaultScope, error) {
	invalid := &envStateDecodeFailure{message: "server returned invalid encrypted ENV scope", cause: environmente2ee.ErrInvalid}
	if len(s.Envelope) > base64.RawURLEncoding.EncodedLen(environmente2ee.MaximumVaultScopeBytes) {
		return environmente2ee.VaultScope{}, invalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s.Envelope)
	if err != nil {
		invalid.cause = err
		return environmente2ee.VaultScope{}, invalid
	}
	public, err := base64.RawURLEncoding.Strict().DecodeString(s.WriterPublic)
	if err != nil {
		invalid.cause = err
		return environmente2ee.VaultScope{}, invalid
	}
	scope, err := environmente2ee.ParseVaultScope(raw, public)
	if err != nil {
		invalid.cause = err
		return environmente2ee.VaultScope{}, invalid
	}
	c := scope.Claims
	if c.WorkspaceID != s.WorkspaceID || c.OwnerKind != s.OwnerKind || c.OwnerID != s.OwnerID || c.MachineID != s.MachineID || c.KeyEpoch != s.KeyEpoch || c.Revision != s.Revision || scope.ID.String() != s.DocumentID {
		return environmente2ee.VaultScope{}, invalid
	}
	return scope, nil
}

func (c *Client) vaultDataRequest(ctx context.Context, method, path string, body, out any) error {
	return c.vaultDataRequestLimited(ctx, method, path, body, out, 0)
}

func (c *Client) vaultDataRequestLimited(ctx context.Context, method, path string, body, out any, maxResponseBytes int64) error {
	var headers http.Header
	if err := c.doRequestMetaLimited(ctx, method, path, body, out, environmentNoStoreRequestHeaders(nil), true, &headers, maxResponseBytes); err != nil {
		return err
	}
	return validateEnvironmentNoStore(headers)
}
func vaultScopePath(kind, owner, machine string) string {
	return "/v1/environment/scopes/" + url.PathEscape(kind) + "/" + url.PathEscape(owner) + "?machine_id=" + url.QueryEscape(machine)
}
func (c *Client) GetVaultScope(ctx context.Context, kind, owner, machine string) (VaultScopeState, error) {
	var out VaultScopeState
	err := c.vaultDataRequest(ctx, http.MethodGet, vaultScopePath(kind, owner, machine), nil, &out)
	if err == nil {
		_, err = DecodeVaultScope(out)
		if err == nil && (out.WorkspaceID != c.Workspace() || out.OwnerKind != kind || out.OwnerID != owner || out.MachineID != machine) {
			err = errors.New("server returned wrong ENV source coordinate")
		}
	}
	return out, err
}
func (c *Client) PutVaultScope(ctx context.Context, kind, owner, machine string, in VaultScopePut) (VaultScopeState, error) {
	var out VaultScopeState
	err := c.vaultDataRequest(ctx, http.MethodPut, vaultScopePath(kind, owner, machine), in, &out)
	if err == nil {
		_, err = DecodeVaultScope(out)
		if err == nil && (out.WorkspaceID != c.Workspace() || out.OwnerKind != kind || out.OwnerID != owner || out.MachineID != machine) {
			err = errors.New("server returned wrong ENV source coordinate")
		}
	}
	return out, err
}
func (c *Client) GetVaultSharing(ctx context.Context, account string) (VaultSharingState, error) {
	scoped := *c
	scoped.workspace = "personal"
	var out VaultSharingState
	err := scoped.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/users/"+url.PathEscape(account)+"/sharing", nil, &out)
	return out, err
}
func (c *Client) GetVaultTeam(ctx context.Context, team string) (VaultTeamState, error) {
	scoped, err := c.teamWorkspace(team)
	if err != nil {
		return VaultTeamState{}, err
	}
	var out VaultTeamState
	err = scoped.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/teams/"+url.PathEscape(team), nil, &out)
	return out, err
}

// teamWorkspace binds a copy of the client to the team a team-ENV operation names: the server refuses
// a team operation sent in another workspace (409 workspace_target_conflict), whatever --workspace the
// command was given, and `pb env resume` replays a pending team operation the same way.
func (c *Client) teamWorkspace(team string) (*Client, error) {
	scoped := *c
	if err := scoped.SetWorkspace(team); err != nil {
		return nil, err
	}
	return &scoped, nil
}

func (c *Client) CreateVaultTeam(ctx context.Context, in VaultTeamCreate) (VaultTeamState, error) {
	scoped, err := c.teamWorkspace(in.TeamID)
	if err != nil {
		return VaultTeamState{}, err
	}
	var out VaultTeamState
	err = scoped.vaultDataRequest(ctx, http.MethodPost, "/v1/environment/teams", in, &out)
	return out, err
}
func (c *Client) GrantVaultTeamMember(ctx context.Context, team string, in VaultMemberGrant) (VaultTeamState, error) {
	scoped, err := c.teamWorkspace(team)
	if err != nil {
		return VaultTeamState{}, err
	}
	var out VaultTeamState
	err = scoped.vaultDataRequest(ctx, http.MethodPost, "/v1/environment/teams/"+url.PathEscape(team)+"/members", in, &out)
	return out, err
}
func (c *Client) RotateVaultTeam(ctx context.Context, team string, in VaultTeamRotate) (VaultTeamState, error) {
	scoped, err := c.teamWorkspace(team)
	if err != nil {
		return VaultTeamState{}, err
	}
	var out VaultTeamState
	err = scoped.vaultDataRequest(ctx, http.MethodPost, "/v1/environment/teams/"+url.PathEscape(team)+"/rotate", in, &out)
	return out, err
}
func (c *Client) GetVaultGrants(ctx context.Context) ([]VaultGrantState, error) {
	scoped := *c
	scoped.workspace = "personal"
	var out struct {
		Grants []VaultGrantState `json:"grants"`
	}
	err := scoped.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/grants", nil, &out)
	return out.Grants, err
}
func (c *Client) AckVaultGrant(ctx context.Context, digest, vaultID string) error {
	scoped := *c
	scoped.workspace = "personal"
	return scoped.vaultDataRequest(ctx, http.MethodPost, "/v1/environment/grants/"+url.PathEscape(digest)+"/ack", struct {
		VaultDocumentID string `json:"vault_document_id"`
	}{vaultID}, nil)
}

func (c *Client) GetVaultPersonalScopes(ctx context.Context) (VaultPersonalScopes, error) {
	personal := *c
	personal.workspace = "personal"
	c = &personal

	var out VaultPersonalScopes
	err := c.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/scopes", nil, &out)
	return out, err
}
func (c *Client) ResetVault(ctx context.Context, in VaultPersonalReset) (PasswordVaultHead, error) {
	personal := *c
	personal.workspace = "personal"
	c = &personal

	var out PasswordVaultHead
	err := c.vaultDataRequest(ctx, http.MethodPost, "/v1/environment-vault/reset", in, &out)
	if err == nil {
		_, _, err = DecodePasswordVaultHead(out)
	}
	return out, err
}

func (c *Client) StageVaultPersonalScope(ctx context.Context, operation string, in VaultPersonalScopeStage) (VaultScopeDocument, error) {
	personal := *c
	personal.workspace = "personal"
	c = &personal

	var out VaultScopeDocument
	err := c.vaultDataRequest(ctx, http.MethodPut, "/v1/environment/rotate-personal/"+url.PathEscape(operation)+"/scopes", in, &out)
	return out, err
}
func (c *Client) AbortVaultPersonalRotation(ctx context.Context, operation string) error {
	personal := *c
	personal.workspace = "personal"
	c = &personal

	return c.vaultDataRequest(ctx, http.MethodDelete, "/v1/environment/rotate-personal/"+url.PathEscape(operation), nil, nil)
}

func (c *Client) RotateVaultPersonal(ctx context.Context, in VaultPersonalRotate) (PasswordVaultHead, error) {
	personal := *c
	personal.workspace = "personal"
	c = &personal

	var out PasswordVaultHead
	err := c.vaultDataRequest(ctx, http.MethodPost, "/v1/environment/rotate-personal", in, &out)
	if err == nil {
		_, _, err = DecodePasswordVaultHead(out)
	}
	return out, err
}

func (c *Client) GetVaultScopeInWorkspace(ctx context.Context, workspace, kind, owner, machine string) (VaultScopeState, error) {
	scoped := *c
	if err := scoped.SetWorkspace(workspace); err != nil {
		return VaultScopeState{}, err
	}
	return scoped.GetVaultScope(ctx, kind, owner, machine)
}
func (c *Client) PutVaultScopeInWorkspace(ctx context.Context, workspace, kind, owner, machine string, in VaultScopePut) (VaultScopeState, error) {
	scoped := *c
	if err := scoped.SetWorkspace(workspace); err != nil {
		return VaultScopeState{}, err
	}
	return scoped.PutVaultScope(ctx, kind, owner, machine, in)
}
