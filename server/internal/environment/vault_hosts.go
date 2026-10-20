package environment

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type VaultHostKeyRequest struct {
	OperationID            string `json:"operation_id"`
	InstallationGeneration uint64 `json:"installation_generation"`
	HostKeyGeneration      uint64 `json:"host_key_generation"`
	HostPublic             string `json:"host_public"`
}

func stringsVaultKeyID(id string) bool {
	if len(id) != 48 || id[:5] != "envk_" {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(id[5:])
	return err == nil && len(raw) == 32
}
func vaultHostKeyID(public []byte) string {
	sum := sha256.Sum256([]byte(`{"crv":"X25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(public) + `"}`))
	return "envk_" + base64.RawURLEncoding.EncodeToString(sum[:])
}
func authorizeVaultHostTx(ctx context.Context, tx *db.Tx, account, machine string) (uint64, error) {
	var generation uint64
	var capabilities []string
	err := tx.QueryRow(ctx, `SELECT installation_generation,configured_capabilities FROM user_machines WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL AND revoked_at IS NULL AND state NOT IN('revoked','deleted','disconnected')`, machine, account).Scan(&generation, &capabilities)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrMachineNotFound
	}
	if err != nil {
		return 0, err
	}
	if !environmentCapable(capabilities) {
		return 0, ErrMachineEnvironmentUnavailable
	}
	return generation, nil
}
func b64VaultPublic(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func vaultAccountConfigurationGenerationTx(ctx context.Context, tx *db.Tx, account string) (uint64, error) {
	var generation uint64
	err := tx.QueryRow(ctx, `SELECT generation FROM environment_vault_account_configurations WHERE account_id=$1`, account).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return generation, err
}

func readVaultRecipientTx(ctx context.Context, tx *db.Tx, account, machine string) (VaultLayerRecipient, error) {
	out := VaultLayerRecipient{RecipientAccount: account, MachineID: machine}
	var public []byte
	err := tx.QueryRow(ctx, `SELECT installation_generation,host_key_generation,host_public FROM environment_vault_hosts WHERE machine_id=$1 AND account_id=$2`, machine, account).Scan(&out.InstallationGeneration, &out.HostKeyGeneration, &public)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.HostPublic = b64VaultPublic(public)
	return out, nil
}

func (s *Service) RegisterVaultHostKey(ctx context.Context, account, machine string, request VaultHostKeyRequest) (VaultLayerRecipient, error) {
	var out VaultLayerRecipient
	public, err := DecodeCanonicalBase64URL(request.HostPublic, 32)
	if err != nil || !validX25519Public(public) || !validCounter(request.InstallationGeneration) || !validCounter(request.HostKeyGeneration) || !validIdentifier(request.OperationID) {
		return out, ErrProtocolInvalid
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		generation, err := authorizeVaultHostTx(ctx, tx, account, machine)
		if err != nil {
			return err
		}
		if generation != request.InstallationGeneration {
			return ErrVersionConflict
		}
		existing, err := readVaultRecipientTx(ctx, tx, account, machine)
		exists := err == nil
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if exists && existing.InstallationGeneration == generation && existing.HostKeyGeneration == request.HostKeyGeneration && existing.HostPublic == request.HostPublic {
			out = existing
			return nil
		}
		if exists && existing.InstallationGeneration == generation && request.HostKeyGeneration != existing.HostKeyGeneration+1 {
			return ErrVersionConflict
		}
		if _, err := tx.Exec(ctx, `INSERT INTO environment_vault_hosts(machine_id,account_id,installation_generation,host_key_generation,host_public) VALUES($1,$2,$3,$4,$5) ON CONFLICT(machine_id) DO UPDATE SET installation_generation=EXCLUDED.installation_generation,host_key_generation=EXCLUDED.host_key_generation,host_public=EXCLUDED.host_public`, machine, account, generation, request.HostKeyGeneration, public); err != nil {
			return err
		}
		out, err = readVaultRecipientTx(ctx, tx, account, machine)
		return err
	})
	return out, err
}
