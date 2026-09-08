package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type PasswordVaultState struct {
	Issuer     string `json:"issuer"`
	AccountID  string `json:"account_id"`
	Generation uint64 `json:"generation"`
	DocumentID string `json:"document_id"`
	Envelope   string `json:"envelope"`
}

func (s PasswordVaultState) Decode() (environmente2ee.VaultHead, []byte, error) {
	invalid := errors.New("server returned invalid ENV vault state")
	if len(s.Envelope) > base64.RawURLEncoding.EncodedLen(environmente2ee.MaximumVaultBytes) {
		return environmente2ee.VaultHead{}, nil, invalid
	}
	id, err := environmente2ee.ParseDocumentID(s.DocumentID)
	if err != nil || s.Issuer == "" || s.AccountID == "" || s.Generation == 0 || s.Generation > environmente2ee.MaximumContractInteger {
		return environmente2ee.VaultHead{}, nil, invalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s.Envelope)
	if err != nil || len(raw) == 0 || len(raw) > environmente2ee.MaximumVaultBytes || environmente2ee.DocumentID(sha256.Sum256(raw)) != id {
		return environmente2ee.VaultHead{}, nil, invalid
	}
	head := environmente2ee.VaultHead{Issuer: s.Issuer, AccountID: s.AccountID, Generation: s.Generation, ID: id}
	parsed, _, err := environmente2ee.InspectPasswordVault(raw)
	if err != nil || parsed != head {
		return environmente2ee.VaultHead{}, nil, invalid
	}
	return head, raw, nil
}

func (c *Client) GetPasswordVault(ctx context.Context) (PasswordVaultState, error) {
	return c.passwordVaultRequest(ctx, http.MethodGet, nil)
}

func (c *Client) PutPasswordVault(ctx context.Context, raw []byte) (PasswordVaultState, error) {
	if len(raw) == 0 || len(raw) > environmente2ee.MaximumVaultBytes {
		return PasswordVaultState{}, errors.New("invalid ENV vault envelope size")
	}
	return c.passwordVaultRequest(ctx, http.MethodPut, struct {
		Envelope string `json:"envelope"`
	}{base64.RawURLEncoding.EncodeToString(raw)})
}

func (c *Client) passwordVaultRequest(ctx context.Context, method string, body any) (PasswordVaultState, error) {
	var state PasswordVaultState
	var headers http.Header
	if err := c.doRequestMeta(ctx, method, "/v1/environment-vault", body, &state, environmentNoStoreRequestHeaders(nil), true, &headers); err != nil {
		return PasswordVaultState{}, err
	}
	if err := validateEnvironmentNoStore(headers); err != nil {
		return PasswordVaultState{}, err
	}
	if _, _, err := state.Decode(); err != nil {
		return PasswordVaultState{}, err
	}
	return state, nil
}
