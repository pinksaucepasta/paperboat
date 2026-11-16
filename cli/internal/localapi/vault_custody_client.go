package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const vaultCustodyMaximumJSONBytes = vaultCustodyMaximumValueBytes*4/3 + 16384
const vaultCustodyHandoffMaximumJSONBytes = vaultCustodyMaximumEntries * vaultCustodyMaximumJSONBytes

// VaultCustody sends secret custody only to the authenticated local daemon
// transport. Request and response bodies are never included in errors.
func (c *Client) VaultCustody(ctx context.Context, value VaultCustodyRequest) (VaultCustodyResponse, error) {
	var out VaultCustodyResponse
	if c == nil || ctx == nil || !validVaultCustodyReference(value.Reference) || !validVaultCustodySession(value.SessionID) || len(value.Value) > vaultCustodyMaximumValueBytes {
		return out, ErrVaultCustodyInvalid
	}
	if err := c.postVaultCustody(ctx, "/v1/vault-custody", value, &out, vaultCustodyMaximumJSONBytes); err != nil {
		clear(out.Value)
		return VaultCustodyResponse{}, err
	}
	if len(out.Value) > vaultCustodyMaximumValueBytes {
		clear(out.Value)
		return VaultCustodyResponse{}, ErrInvalidResponse
	}
	return out, nil
}

// VaultCustodyHandoff exports ("export") the daemon's custody or imports a predecessor's export
// ("import"; the export already carries that operation) during a daemon replacement.
func (c *Client) VaultCustodyHandoff(ctx context.Context, value VaultCustodyHandoff) (VaultCustodyHandoff, error) {
	var out VaultCustodyHandoff
	if c == nil || ctx == nil || value.Operation != "export" && value.Operation != "import" {
		return out, ErrVaultCustodyInvalid
	}
	if err := c.postVaultCustody(ctx, "/v1/vault-custody/handoff", value, &out, vaultCustodyHandoffMaximumJSONBytes); err != nil {
		out.Clear()
		return VaultCustodyHandoff{}, err
	}
	return out, nil
}

func (c *Client) postVaultCustody(ctx context.Context, path string, value, out any, limit int64) error {
	body, err := json.Marshal(value)
	if err != nil || int64(len(body)) > limit {
		clear(body)
		return ErrVaultCustodyInvalid
	}
	defer clear(body)
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "http://paperboat.local"+path, bytes.NewReader(body))
	if err != nil {
		return ErrVaultCustodyInvalid
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	request.Header.Set("X-Paperboat-Request-ID", localRequestID())
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		remote := decodeRemoteErrorReader(response.StatusCode, io.LimitReader(response.Body, maxJSONBytes+1))
		var typed *RemoteError
		if errors.As(remote, &typed) && typed.Code == "vault_custody_stale" {
			return ErrVaultCustodyStale
		}
		if errors.As(remote, &typed) && typed.Code == "vault_custody_limit" {
			return ErrVaultCustodyLimit
		}
		return remote
	}
	if response.Header.Get("X-Paperboat-Protocol") != ProtocolV1 {
		return ErrVersionMismatch
	}
	if response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Cache-Control") != "no-store" || decodeStrictJSON(io.LimitReader(response.Body, limit+1), out) != nil {
		return ErrInvalidResponse
	}
	return nil
}
