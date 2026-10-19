package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func (c *Client) ManagedSSHTarget(ctx context.Context, machineID string, generation uint64) (ManagedSSHTarget, error) {
	if strings.TrimSpace(machineID) == "" || generation == 0 {
		return ManagedSSHTarget{}, errors.New("valid managed SSH target identity is required")
	}
	var target ManagedSSHTarget
	path := fmt.Sprintf("/v1/machines/%s/ssh-target?machine_generation=%d", url.PathEscape(machineID), generation)
	if err := c.do(ctx, http.MethodGet, path, nil, &target); err != nil {
		return ManagedSSHTarget{}, err
	}
	if target.Type != "machine_target" || target.Version != 1 || target.MachineID != machineID || target.MachineGeneration != generation || target.Port == 0 || target.ReconciliationVersion == 0 || strings.TrimSpace(target.OSUser) == "" {
		return ManagedSSHTarget{}, errors.New("paperboat-server returned an invalid managed SSH target")
	}
	return target, nil
}

func (c *Client) RegisterManagedSSHTarget(ctx context.Context, machineID string, generation uint64, osUser string, port uint16, operationID string) (ManagedSSHTarget, error) {
	if strings.TrimSpace(machineID) == "" || generation == 0 || strings.TrimSpace(osUser) == "" || port == 0 || strings.TrimSpace(operationID) == "" {
		return ManagedSSHTarget{}, errors.New("valid managed SSH target registration is required")
	}
	var target ManagedSSHTarget
	err := c.doWithHeaders(ctx, http.MethodPut, "/v1/machines/"+url.PathEscape(machineID)+"/ssh-target", map[string]any{"machine_generation": generation, "os_user": osUser, "port": port}, &target, http.Header{"Idempotency-Key": []string{operationID}})
	if err != nil {
		return ManagedSSHTarget{}, err
	}
	if target.Type != "machine_target" || target.Version != 1 || target.MachineID != machineID || target.MachineGeneration != generation || target.OSUser != osUser || target.Port != port || target.ReconciliationVersion == 0 {
		return ManagedSSHTarget{}, errors.New("paperboat-server returned an invalid managed SSH target")
	}
	return target, nil
}

func (c *Client) UpdateManagedSSHTargetPort(ctx context.Context, machineID string, generation, expectedVersion uint64, port uint16, operationID string) (ManagedSSHTarget, error) {
	if strings.TrimSpace(machineID) == "" || generation == 0 || expectedVersion == 0 || port == 0 || strings.TrimSpace(operationID) == "" {
		return ManagedSSHTarget{}, errors.New("valid managed SSH target update is required")
	}
	var target ManagedSSHTarget
	err := c.doWithHeaders(ctx, http.MethodPut, "/v1/machines/"+url.PathEscape(machineID)+"/ssh-target", map[string]any{"machine_generation": generation, "port": port, "expected_reconciliation_version": expectedVersion}, &target, http.Header{"Idempotency-Key": []string{operationID}})
	if err != nil {
		return ManagedSSHTarget{}, err
	}
	if target.Type != "machine_target" || target.Version != 1 || target.MachineID != machineID || target.MachineGeneration != generation || target.Port != port || target.ReconciliationVersion != expectedVersion+1 {
		return ManagedSSHTarget{}, errors.New("paperboat-server returned an invalid managed SSH target update")
	}
	return target, nil
}

func (c *Client) ObserveManagedSSHHostKeys(ctx context.Context, machineID, keyID, operationID string, generation, observationGeneration uint64, publicKeys []string, proof []byte) (ManagedSSHHostKeySet, error) {
	if strings.TrimSpace(machineID) == "" || strings.TrimSpace(keyID) == "" || strings.TrimSpace(operationID) == "" || generation == 0 || observationGeneration == 0 || len(publicKeys) == 0 || len(proof) == 0 {
		return ManagedSSHHostKeySet{}, errors.New("valid managed SSH host-key observation is required")
	}
	var set ManagedSSHHostKeySet
	err := c.doWithHeaders(ctx, http.MethodPut, "/v1/machines/"+url.PathEscape(machineID)+"/ssh-host-keys", map[string]any{"observation_generation": observationGeneration, "public_keys": publicKeys}, &set, http.Header{"X-Paperboat-Machine-Identity": []string{keyID}, "X-Paperboat-Machine-Proof": []string{base64.RawURLEncoding.EncodeToString(proof)}})
	if err != nil {
		return ManagedSSHHostKeySet{}, err
	}
	if set.Type != "host_key_set" || set.Version != 1 || !validManagedSSHHostKeySetID(set.SetID) || set.MachineID != machineID || set.MachineGeneration != generation || set.ObservationGeneration == 0 || len(set.Keys) == 0 || set.ReconciliationVersion == 0 {
		return ManagedSSHHostKeySet{}, errors.New("paperboat-server returned an invalid managed SSH host-key observation")
	}
	return set, nil
}

func (c *Client) ManagedSSHAuthorizedKeys(ctx context.Context, machineID, keyID string, generation uint64, proof []byte) (ManagedSSHAuthorizedKeys, error) {
	if strings.TrimSpace(machineID) == "" || strings.TrimSpace(keyID) == "" || generation == 0 || len(proof) == 0 {
		return ManagedSSHAuthorizedKeys{}, errors.New("valid managed SSH authorized-key request is required")
	}
	var set ManagedSSHAuthorizedKeys
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/ssh-authorized-keys", map[string]any{}, &set, http.Header{"X-Paperboat-Machine-Identity": []string{keyID}, "X-Paperboat-Machine-Proof": []string{base64.RawURLEncoding.EncodeToString(proof)}})
	if err != nil {
		return ManagedSSHAuthorizedKeys{}, err
	}
	if set.Type != "authorized_key_set" || set.Version != 1 || set.MachineID != machineID || set.MachineGeneration != generation || len(set.Keys) > 64 {
		return ManagedSSHAuthorizedKeys{}, errors.New("paperboat-server returned an invalid managed SSH authorized-key set")
	}
	return set, nil
}

func (c *Client) ManagedSSHHostKeys(ctx context.Context, machineID string, generation uint64) (ManagedSSHHostKeySet, error) {
	return c.managedSSHHostKeys(ctx, machineID, generation, "active")
}

func (c *Client) ManagedSSHPendingHostKeys(ctx context.Context, machineID string, generation uint64) (ManagedSSHHostKeySet, error) {
	return c.managedSSHHostKeys(ctx, machineID, generation, "pending")
}

func (c *Client) managedSSHHostKeys(ctx context.Context, machineID string, generation uint64, state string) (ManagedSSHHostKeySet, error) {
	if strings.TrimSpace(machineID) == "" || generation == 0 {
		return ManagedSSHHostKeySet{}, errors.New("valid managed SSH host-key identity is required")
	}
	var set ManagedSSHHostKeySet
	path := fmt.Sprintf("/v1/machines/%s/ssh-host-keys?machine_generation=%d&state=%s", url.PathEscape(machineID), generation, state)
	if err := c.do(ctx, http.MethodGet, path, nil, &set); err != nil {
		return ManagedSSHHostKeySet{}, err
	}
	if set.Type != "host_key_set" || set.Version != 1 || set.SetID == "" || set.MachineID != machineID || set.MachineGeneration != generation || set.ObservationGeneration == 0 || len(set.Keys) == 0 || set.State != state || set.Fingerprint == "" || set.ReconciliationVersion == 0 {
		return ManagedSSHHostKeySet{}, errors.New("paperboat-server returned an invalid managed SSH host-key set")
	}
	return set, nil
}

func (c *Client) PromoteManagedSSHHostKeys(ctx context.Context, machineID, setID, fingerprint, operationID string, generation uint64) (ManagedSSHHostKeySet, error) {
	if strings.TrimSpace(machineID) == "" || strings.TrimSpace(fingerprint) == "" || strings.TrimSpace(operationID) == "" || generation == 0 {
		return ManagedSSHHostKeySet{}, errors.New("valid managed SSH host-key promotion is required")
	}
	var set ManagedSSHHostKeySet
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/ssh-host-keys/"+url.PathEscape(setID)+"/promote", map[string]any{"machine_generation": generation, "expected_fingerprint": fingerprint}, &set, http.Header{"Idempotency-Key": []string{operationID}})
	if err != nil {
		return ManagedSSHHostKeySet{}, err
	}
	if set.Type != "host_key_set" || set.Version != 1 || !validManagedSSHHostKeySetID(set.SetID) || set.MachineID != machineID || set.MachineGeneration != generation || set.State != "active" || set.Fingerprint != fingerprint || set.ReconciliationVersion == 0 {
		return ManagedSSHHostKeySet{}, errors.New("paperboat-server returned an invalid managed SSH host-key promotion")
	}
	return set, nil
}

func (c *Client) RegisterManagedSSHClientKey(ctx context.Context, publicKey string, fingerprint [32]byte, operationID string) (ManagedSSHClientKey, error) {
	publicKey = strings.TrimSpace(publicKey)
	operationID = strings.TrimSpace(operationID)
	if publicKey == "" || fingerprint == [32]byte{} || operationID == "" {
		return ManagedSSHClientKey{}, errors.New("valid managed SSH client-key registration is required")
	}
	encodedFingerprint := "SHA256:" + base64.RawStdEncoding.EncodeToString(fingerprint[:])
	var key ManagedSSHClientKey
	path := "/v1/ssh/client-keys/" + url.PathEscape(encodedFingerprint)
	err := c.doWithHeaders(ctx, http.MethodPut, path, map[string]string{"public_key": publicKey}, &key, http.Header{"Idempotency-Key": []string{operationID}})
	if err != nil {
		return ManagedSSHClientKey{}, err
	}
	if key.Type != "client_key" || key.Version != 1 || key.Fingerprint != encodedFingerprint || key.PublicKey != publicKey || key.State != "active" || key.ReconciliationVersion == 0 {
		return ManagedSSHClientKey{}, errors.New("paperboat-server returned an invalid managed SSH client key")
	}
	return key, nil
}
