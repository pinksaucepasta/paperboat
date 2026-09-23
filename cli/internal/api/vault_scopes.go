package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type VaultScopeState struct {
	OwnerKind    string `json:"owner_kind"`
	OwnerID      string `json:"owner_id"`
	MachineID    string `json:"machine_id"`
	KeyEpoch     uint64 `json:"key_epoch"`
	Revision     uint64 `json:"revision"`
	DocumentID   string `json:"document_id"`
	Envelope     string `json:"envelope"`
	WriterPublic string `json:"writer_public"`
}

func (s VaultScopeState) Decode() (environmente2ee.VaultScope, error) {
	invalid := errors.New("server returned invalid encrypted ENV scope")
	if len(s.Envelope) > base64.RawURLEncoding.EncodedLen(environmente2ee.MaximumVaultScopeBytes) {
		return environmente2ee.VaultScope{}, invalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s.Envelope)
	if err != nil {
		return environmente2ee.VaultScope{}, invalid
	}
	public, err := base64.RawURLEncoding.Strict().DecodeString(s.WriterPublic)
	if err != nil {
		return environmente2ee.VaultScope{}, invalid
	}
	scope, err := environmente2ee.ParseVaultScope(raw, public)
	if err != nil {
		return environmente2ee.VaultScope{}, invalid
	}
	c := scope.Claims
	if c.OwnerKind != s.OwnerKind || c.OwnerID != s.OwnerID || c.MachineID != s.MachineID || c.KeyEpoch != s.KeyEpoch || c.Revision != s.Revision || scope.ID.String() != s.DocumentID {
		return environmente2ee.VaultScope{}, invalid
	}
	return scope, nil
}

type VaultScopePut struct {
	OperationID string `json:"operation_id"`
	Envelope    string `json:"envelope"`
}
type VaultSharingState struct {
	AccountID       string `json:"account_id"`
	VaultGeneration uint64 `json:"vault_generation"`
	WriterPublic    string `json:"writer_public"`
	SharingPublic   string `json:"sharing_public"`
}
type VaultTeamMember struct {
	AccountID            string `json:"account_id"`
	MembershipGeneration uint64 `json:"membership_generation"`
	Role                 string `json:"role"`
	Active               bool   `json:"active"`
	GrantEpoch           uint64 `json:"grant_epoch"`
	EnvPermission        string `json:"env_permission"`
}
type VaultTeamState struct {
	RotationRequired bool              `json:"rotation_required"`
	TeamID           string            `json:"team_id"`
	OwnerAccount     string            `json:"owner_account"`
	Generation       uint64            `json:"generation"`
	KeyEpoch         uint64            `json:"key_epoch"`
	Members          []VaultTeamMember `json:"members"`
	Scope            VaultScopeState   `json:"scope"`
}
type VaultTeamCreate struct {
	VaultEnvelope          string `json:"vault_envelope"`
	OperationID            string `json:"operation_id"`
	TeamID                 string `json:"team_id"`
	ScopeEnvelope          string `json:"scope_envelope"`
	GrantEnvelope          string `json:"grant_envelope"`
	ExpectedTeamGeneration uint64 `json:"expected_team_generation"`
}
type VaultMemberGrant struct {
	OperationID                  string `json:"operation_id"`
	AccountID                    string `json:"account_id"`
	ExpectedTeamGeneration       uint64 `json:"expected_team_generation"`
	ExpectedMembershipGeneration uint64 `json:"expected_membership_generation"`
	GrantEnvelope                string `json:"grant_envelope"`
}
type VaultTeamRotate struct {
	ConfirmTotalLoss       bool     `json:"confirm_total_loss"`
	VaultEnvelope          string   `json:"vault_envelope"`
	OperationID            string   `json:"operation_id"`
	ExpectedTeamGeneration uint64   `json:"expected_team_generation"`
	RemoveAccountIDs       []string `json:"remove_account_ids"`
	ScopeEnvelope          string   `json:"scope_envelope"`
	GrantEnvelopes         []string `json:"grant_envelopes"`
}
type VaultGrantState struct {
	TeamID               string `json:"team_id"`
	MembershipGeneration uint64 `json:"membership_generation"`
	TeamEpoch            uint64 `json:"team_epoch"`
	DocumentID           string `json:"document_id"`
	Envelope             string `json:"envelope"`
	SenderWriterPublic   string `json:"sender_writer_public"`
}

func (c *Client) vaultDataRequest(ctx context.Context, method, path string, body, out any) error {
	var headers http.Header
	if err := c.doRequestMeta(ctx, method, path, body, out, environmentNoStoreRequestHeaders(nil), true, &headers); err != nil {
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
		_, err = out.Decode()
	}
	return out, err
}
func (c *Client) PutVaultScope(ctx context.Context, kind, owner, machine string, in VaultScopePut) (VaultScopeState, error) {
	var out VaultScopeState
	err := c.vaultDataRequest(ctx, http.MethodPut, vaultScopePath(kind, owner, machine), in, &out)
	if err == nil {
		_, err = out.Decode()
	}
	return out, err
}
func (c *Client) GetVaultSharing(ctx context.Context, account string) (VaultSharingState, error) {
	var out VaultSharingState
	err := c.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/users/"+url.PathEscape(account)+"/sharing", nil, &out)
	return out, err
}
func (c *Client) GetVaultTeam(ctx context.Context, team string) (VaultTeamState, error) {
	var out VaultTeamState
	err := c.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/teams/"+url.PathEscape(team), nil, &out)
	return out, err
}
func (c *Client) CreateVaultTeam(ctx context.Context, in VaultTeamCreate) (VaultTeamState, error) {
	var out VaultTeamState
	err := c.vaultDataRequest(ctx, http.MethodPost, "/v1/environment/teams", in, &out)
	return out, err
}
func (c *Client) GrantVaultTeamMember(ctx context.Context, team string, in VaultMemberGrant) (VaultTeamState, error) {
	var out VaultTeamState
	err := c.vaultDataRequest(ctx, http.MethodPost, "/v1/environment/teams/"+url.PathEscape(team)+"/members", in, &out)
	return out, err
}
func (c *Client) RotateVaultTeam(ctx context.Context, team string, in VaultTeamRotate) (VaultTeamState, error) {
	var out VaultTeamState
	err := c.vaultDataRequest(ctx, http.MethodPost, "/v1/environment/teams/"+url.PathEscape(team)+"/rotate", in, &out)
	return out, err
}
func (c *Client) GetVaultGrants(ctx context.Context) ([]VaultGrantState, error) {
	var out struct {
		Grants []VaultGrantState `json:"grants"`
	}
	err := c.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/grants", nil, &out)
	return out.Grants, err
}
func (c *Client) AckVaultGrant(ctx context.Context, digest, vaultID string) error {
	return c.vaultDataRequest(ctx, http.MethodPost, "/v1/environment/grants/"+url.PathEscape(digest)+"/ack", struct {
		VaultDocumentID string `json:"vault_document_id"`
	}{vaultID}, nil)
}

type VaultPersonalInventory struct {
	RotationRequired bool              `json:"rotation_required"`
	KeyEpoch         uint64            `json:"key_epoch"`
	Scopes           []VaultScopeState `json:"scopes"`
}
type VaultReset struct {
	OperationID             string   `json:"operation_id"`
	ExpectedVaultDocumentID string   `json:"expected_vault_document_id"`
	VaultEnvelope           string   `json:"vault_envelope"`
	ScopeEnvelopes          []string `json:"scope_envelopes"`
	ConfirmTotalLoss        bool     `json:"confirm_total_loss"`
}

func (c *Client) GetVaultPersonalScopes(ctx context.Context) (VaultPersonalInventory, error) {
	var out VaultPersonalInventory
	err := c.vaultDataRequest(ctx, http.MethodGet, "/v1/environment/scopes", nil, &out)
	return out, err
}
func (c *Client) ResetVault(ctx context.Context, in VaultReset) (PasswordVaultState, error) {
	var out PasswordVaultState
	err := c.vaultDataRequest(ctx, http.MethodPost, "/v1/environment-vault/reset", in, &out)
	if err == nil {
		_, _, err = out.Decode()
	}
	return out, err
}

type VaultPersonalRotate struct {
	OperationID             string               `json:"operation_id"`
	ExpectedVaultDocumentID string               `json:"expected_vault_document_id"`
	VaultEnvelope           string               `json:"vault_envelope"`
	ScopeDocuments          []VaultScopeDocument `json:"scope_documents"`
}
type VaultScopeDocument struct {
	MachineID  string `json:"machine_id"`
	DocumentID string `json:"document_id"`
}
type VaultPersonalScopeStage struct {
	ExpectedVaultDocumentID string `json:"expected_vault_document_id"`
	MachineID               string `json:"machine_id"`
	Envelope                string `json:"envelope"`
}

func (c *Client) StageVaultPersonalScope(ctx context.Context, operation string, in VaultPersonalScopeStage) (VaultScopeDocument, error) {
	var out VaultScopeDocument
	err := c.vaultDataRequest(ctx, http.MethodPut, "/v1/environment/rotate-personal/"+url.PathEscape(operation)+"/scopes", in, &out)
	return out, err
}
func (c *Client) AbortVaultPersonalRotation(ctx context.Context, operation string) error {
	return c.vaultDataRequest(ctx, http.MethodDelete, "/v1/environment/rotate-personal/"+url.PathEscape(operation), nil, nil)
}

func (c *Client) RotateVaultPersonal(ctx context.Context, in VaultPersonalRotate) (PasswordVaultState, error) {
	var out PasswordVaultState
	err := c.vaultDataRequest(ctx, http.MethodPost, "/v1/environment/rotate-personal", in, &out)
	if err == nil {
		_, _, err = out.Decode()
	}
	return out, err
}
