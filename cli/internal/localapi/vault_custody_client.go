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

// VaultCustody sends secret custody only to the authenticated local daemon
// transport. Request and response bodies are never included in errors.
func (c *Client) VaultCustody(ctx context.Context, value VaultCustodyRequest) (VaultCustodyResponse, error) {
	var out VaultCustodyResponse
	if c == nil || ctx == nil || !validVaultCustodyReference(value.Reference) || !validVaultCustodySession(value.SessionID) || len(value.Value) > vaultCustodyMaximumValueBytes {
		return out, ErrVaultCustodyInvalid
	}
	body, err := json.Marshal(value)
	if err != nil || len(body) > vaultCustodyMaximumJSONBytes {
		clear(body)
		return out, ErrVaultCustodyInvalid
	}
	defer clear(body)
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "http://paperboat.local/v1/vault-custody", bytes.NewReader(body))
	if err != nil {
		return out, ErrVaultCustodyInvalid
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	request.Header.Set("X-Paperboat-Request-ID", localRequestID())
	response, err := c.http.Do(request)
	if err != nil {
		return out, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		remote := decodeRemoteErrorReader(response.StatusCode, io.LimitReader(response.Body, maxJSONBytes+1))
		var typed *RemoteError
		if errors.As(remote, &typed) && typed.Code == "vault_custody_stale" {
			return out, ErrVaultCustodyStale
		}
		if errors.As(remote, &typed) && typed.Code == "vault_custody_limit" {
			return out, ErrVaultCustodyLimit
		}
		return out, remote
	}
	if response.Header.Get("X-Paperboat-Protocol") != ProtocolV1 {
		return out, ErrVersionMismatch
	}
	if response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Cache-Control") != "no-store" || decodeStrictJSON(io.LimitReader(response.Body, vaultCustodyMaximumJSONBytes+1), &out) != nil || len(out.Value) > vaultCustodyMaximumValueBytes {
		clear(out.Value)
		return VaultCustodyResponse{}, ErrInvalidResponse
	}
	return out, nil
}
