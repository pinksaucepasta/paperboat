package environment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

const MaximumVaultLayerBytes = (256 << 10) + (64 << 10)

type vaultLayerEnvelope struct {
	_          struct{} `cbor:",toarray"`
	Claims     []byte
	Enc        []byte
	Ciphertext []byte
	Signature  []byte
}

type VaultLayerCoordinate struct {
	WorkspaceID string `json:"workspace_id"`
	OwnerKind   string `json:"owner_kind"`
	OwnerID     string `json:"owner_id"`
	MachineID   string `json:"machine_id"`
}
type VaultLayerSource struct {
	VaultLayerCoordinate
	KeyEpoch   uint64 `json:"key_epoch"`
	Revision   uint64 `json:"revision"`
	DocumentID string `json:"document_id"`
}
type VaultLayerRecipient struct {
	RecipientAccount       string `json:"recipient_account"`
	MachineID              string `json:"machine_id"`
	InstallationGeneration uint64 `json:"installation_generation"`
	HostKeyGeneration      uint64 `json:"host_key_generation"`
	HostPublic             string `json:"host_public"`
	DeliveryGeneration     uint64 `json:"delivery_generation"`
	DocumentID             string `json:"document_id"`
	FenceGeneration        uint64 `json:"fence_generation"`
}
type VaultLayerDelivery struct {
	RecordSequence uint64                 `json:"record_sequence"`
	Observation    *VaultLayerObservation `json:"observation"`
	Applied        bool                   `json:"applied"`
	Recipient      VaultLayerRecipient    `json:"recipient"`
	Source         VaultLayerSource       `json:"source"`
	WriterAccount  string                 `json:"writer_account"`
	WriterPublic   string                 `json:"writer_public"`
	Envelope       string                 `json:"envelope"`
	State          string                 `json:"state"`
}
type VaultLayerPut struct {
	OperationID string `json:"operation_id"`
	Envelope    string `json:"envelope"`
}
type VaultLayerContext struct {
	Recipient      VaultLayerRecipient  `json:"recipient"`
	WorkspaceID    string               `json:"workspace_id"`
	ActorAccountID string               `json:"actor_account_id"`
	MachineID      string               `json:"machine_id"`
	Layers         []VaultLayerDelivery `json:"layers"`
}
type vaultLayerSourceClaims struct {
	_           struct{} `cbor:",toarray"`
	WorkspaceID string
	OwnerKind   string
	OwnerID     string
	MachineID   string
	KeyEpoch    uint64
	Revision    uint64
	Digest      []byte
}
type vaultLayerClaims struct {
	_                      struct{} `cbor:",toarray"`
	Domain                 string
	Version                uint64
	Issuer                 string
	RecipientAccount       string
	MachineID              string
	InstallationGeneration uint64
	HostKeyGeneration      uint64
	HostPublic             []byte
	DeliveryGeneration     uint64
	Previous               []byte
	FenceGeneration        uint64
	Source                 vaultLayerSourceClaims
	WriterAccount          string
	WriterVaultGeneration  uint64
	WriterPublic           []byte
}

func layerCoordinate(c vaultLayerSourceClaims) VaultLayerCoordinate {
	return VaultLayerCoordinate{c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID}
}
func validLayerCoordinate(c VaultLayerCoordinate) bool {
	return validIdentifier(c.WorkspaceID) && validIdentifier(c.OwnerID) && ((c.OwnerKind == "personal" && (c.MachineID == "" || validIdentifier(c.MachineID))) || (c.OwnerKind == "team" && c.WorkspaceID == c.OwnerID && c.WorkspaceID != "personal" && c.MachineID == ""))
}
func parseVaultLayer(raw []byte, issuer string, writer PasswordVaultMetadata) (vaultLayerClaims, error) {
	var e vaultLayerEnvelope
	var c vaultLayerClaims
	if !decodeVaultCanonical(raw, MaximumVaultLayerBytes, &e) || !decodeVaultCanonical(e.Claims, 4096, &c) || c.Domain != "paperboat.environment.host-layer" || c.Version != 1 || c.Issuer != issuer || !validLayerCoordinate(layerCoordinate(c.Source)) || !validIdentifier(c.RecipientAccount) || !validIdentifier(c.MachineID) || !validCounter(c.InstallationGeneration) || !validCounter(c.HostKeyGeneration) || !validX25519Public(c.HostPublic) || !validCounter(c.DeliveryGeneration) || len(c.Previous) != 32 || (c.DeliveryGeneration == 1) != allZeroBytes(c.Previous) || !validCounter(c.FenceGeneration) || !validCounter(c.Source.KeyEpoch) || !validCounter(c.Source.Revision) || len(c.Source.Digest) != 32 || allZeroBytes(c.Source.Digest) || (c.Source.MachineID != "" && c.Source.MachineID != c.MachineID) || c.WriterAccount != writer.Head.AccountID || !validCounter(c.WriterVaultGeneration) || c.WriterVaultGeneration > writer.Head.Generation || !bytes.Equal(c.WriterPublic, writer.WriterPublicKey[:]) || (c.Source.OwnerKind == "personal" && (c.Source.OwnerID != c.WriterAccount || c.RecipientAccount != c.Source.OwnerID)) || len(e.Enc) != 32 || len(e.Ciphertext) <= 16 || len(e.Ciphertext) > (256<<10)+16 || len(e.Signature) != 64 {
		return c, ErrProtocolInvalid
	}
	message, err := canonicalEncoding.Marshal([]any{c.Domain + "-signature", uint64(1), e.Claims, e.Enc, e.Ciphertext})
	if err != nil || !ed25519.Verify(writer.WriterPublicKey[:], message, e.Signature) {
		return c, ErrProtocolSignature
	}
	return c, nil
}

// Workspace membership authorizes shared Team injection. It never grants access
// to another member's private layers or adds terminal/SSH execution authority.
func requireVaultWorkspaceMemberTx(ctx context.Context, tx *db.Tx, account, workspace string) error {
	var active, paid bool
	if !validIdentifier(account) || !validIdentifier(workspace) || workspace == "personal" {
		return ErrInvalidScope
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams t JOIN team_members m USING(team_id) JOIN users u ON u.id=m.account_id WHERE t.team_id=$1 AND m.account_id=$2 AND m.active AND u.status='active' AND t.deleted_at IS NULL AND NOT t.suspended),team_paid_entitled($1)`, workspace, account).Scan(&active, &paid); err != nil {
		return err
	}
	if !active {
		return ErrKeyAuthorizationRequired
	}
	if !paid {
		return ErrTeamEntitlementRequired
	}
	return nil
}
func vaultLayerWriterTx(ctx context.Context, tx *db.Tx, account string, c VaultLayerCoordinate) (bool, error) {
	if !validLayerCoordinate(c) {
		return false, ErrInvalidScope
	}
	if c.OwnerKind == "personal" {
		_, err := authorizeVaultScopeWorkspaceTx(ctx, tx, account, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID)
		return true, err
	}
	if _, err := authorizeVaultScopeTx(ctx, tx, account, "team", c.OwnerID, ""); err != nil {
		return false, err
	}
	team, err := readVaultTeamTx(ctx, tx, c.OwnerID)
	if err != nil {
		return false, err
	}
	member, ok := vaultMember(team, account)
	return ok && member.ENVPermission == "write", nil
}
func readVaultLayerTx(ctx context.Context, tx *db.Tx, machine string, c VaultLayerCoordinate) (VaultLayerDelivery, error) {
	out := VaultLayerDelivery{Source: VaultLayerSource{VaultLayerCoordinate: c}, State: "pending"}
	var public, writer, raw []byte
	var generation, hostGeneration uint64
	var current bool
	err := tx.QueryRow(ctx, `SELECT m.user_id,m.installation_generation,h.host_key_generation,h.host_public,s.key_epoch,s.revision,s.document_id,f.generation,coalesce(l.delivery_generation,0),coalesce(l.document_id,''),coalesce(l.writer_account,''),coalesce(l.writer_public,''::bytea),coalesce(l.envelope,''::bytea),coalesce(l.installation_generation,0),coalesce(l.host_key_generation,0),coalesce(l.applied_fence_generation,0)=f.generation FROM user_machines m JOIN environment_vault_hosts h ON h.machine_id=m.id JOIN environment_vault_scopes s ON s.workspace_id=$2 AND s.owner_kind=$3 AND s.owner_id=$4 AND s.machine_id=$5 JOIN environment_vault_layer_fences f ON f.workspace_id=s.workspace_id AND f.owner_kind=s.owner_kind AND f.owner_id=s.owner_id AND f.source_machine_id=s.machine_id LEFT JOIN environment_vault_layers l ON l.machine_id=m.id AND l.workspace_id=s.workspace_id AND l.owner_kind=s.owner_kind AND l.owner_id=s.owner_id AND l.source_machine_id=s.machine_id WHERE m.id=$1 AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.state NOT IN('deleted','revoked') AND h.installation_generation=m.installation_generation AND 'environment_injection'=ANY(m.configured_capabilities)`, machine, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID).Scan(&out.Recipient.RecipientAccount, &out.Recipient.InstallationGeneration, &out.Recipient.HostKeyGeneration, &public, &out.Source.KeyEpoch, &out.Source.Revision, &out.Source.DocumentID, &out.Recipient.FenceGeneration, &out.Recipient.DeliveryGeneration, &out.Recipient.DocumentID, &out.WriterAccount, &writer, &raw, &generation, &hostGeneration, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT sequence FROM environment_vault_record_heads WHERE workspace_id=$1 AND owner_kind=$2 AND owner_id=$3 AND machine_id=$4 AND key_epoch=$5),0)`, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, out.Source.KeyEpoch).Scan(&out.RecordSequence); err != nil {
		return out, err
	}
	out.Recipient.MachineID = machine
	out.Recipient.HostPublic = b64VaultPublic(public)
	out.WriterPublic = b64VaultPublic(writer)
	out.Envelope = base64.RawURLEncoding.EncodeToString(raw)
	if current && generation == out.Recipient.InstallationGeneration && hostGeneration == out.Recipient.HostKeyGeneration && len(raw) > 0 {
		out.State = "ready"
	}
	if err := layerObservationTx(ctx, tx, &out); err != nil {
		return out, err
	}
	return out, nil
}
func (s *Service) PutVaultLayer(ctx context.Context, account, workspace, machine string, in VaultLayerPut) (VaultLayerDelivery, error) {
	var out VaultLayerDelivery
	raw, err := DecodeCanonicalBase64URL(in.Envelope, MaximumVaultLayerBytes)
	if err != nil {
		return out, err
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		if err := allowVaultWritesTx(ctx, tx, account); err != nil {
			return err
		}
		writer, err := s.vaultMetadataTx(ctx, tx, account)
		if err != nil {
			return err
		}
		claims, err := parseVaultLayer(raw, s.passwordVaultIssuer, writer)
		if err != nil {
			return err
		}
		c := layerCoordinate(claims.Source)
		if c.WorkspaceID != workspace || claims.MachineID != machine {
			return ErrKeyAuthorizationRequired
		}
		writable, err := vaultLayerWriterTx(ctx, tx, account, c)
		if err != nil {
			return err
		}
		current, err := readVaultLayerTx(ctx, tx, machine, c)
		if err != nil {
			return err
		}
		if c.OwnerKind == "personal" && current.Recipient.RecipientAccount != account {
			return ErrKeyAuthorizationRequired
		}
		if c.OwnerKind == "team" {
			if err := requireVaultWorkspaceMemberTx(ctx, tx, current.Recipient.RecipientAccount, workspace); err != nil {
				return err
			}
			if current.Recipient.RecipientAccount != account && !writable {
				return ErrKeyAuthorizationRequired
			}
		}
		replay, digest, err := vaultOperationTx(ctx, tx, account, in.OperationID, "host-layer:"+machine, in, &out)
		if err != nil || replay {
			return err
		}
		r := current.Recipient
		if claims.RecipientAccount != r.RecipientAccount || claims.InstallationGeneration != r.InstallationGeneration || claims.HostKeyGeneration != r.HostKeyGeneration || b64VaultPublic(claims.HostPublic) != r.HostPublic || claims.FenceGeneration != r.FenceGeneration || claims.Source.KeyEpoch != current.Source.KeyEpoch || claims.Source.Revision != current.Source.Revision || digestText(claims.Source.Digest) != current.Source.DocumentID || claims.DeliveryGeneration != r.DeliveryGeneration+1 || (r.DeliveryGeneration > 0 && digestText(claims.Previous) != r.DocumentID) {
			return ErrVersionConflict
		}
		id := sha256.Sum256(raw)
		_, err = tx.Exec(ctx, `INSERT INTO environment_vault_layers(machine_id,workspace_id,owner_kind,owner_id,source_machine_id,delivery_generation,document_id,installation_generation,host_key_generation,applied_fence_generation,writer_account,writer_public,envelope) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT(machine_id,workspace_id,owner_kind,owner_id,source_machine_id) DO UPDATE SET delivery_generation=EXCLUDED.delivery_generation,document_id=EXCLUDED.document_id,installation_generation=EXCLUDED.installation_generation,host_key_generation=EXCLUDED.host_key_generation,applied_fence_generation=EXCLUDED.applied_fence_generation,writer_account=EXCLUDED.writer_account,writer_public=EXCLUDED.writer_public,envelope=EXCLUDED.envelope,observation_seq=CASE WHEN environment_vault_layers.installation_generation<>EXCLUDED.installation_generation OR environment_vault_layers.host_key_generation<>EXCLUDED.host_key_generation THEN 0 ELSE environment_vault_layers.observation_seq END,observation_digest=CASE WHEN environment_vault_layers.installation_generation<>EXCLUDED.installation_generation OR environment_vault_layers.host_key_generation<>EXCLUDED.host_key_generation THEN NULL ELSE environment_vault_layers.observation_digest END,observation=CASE WHEN environment_vault_layers.installation_generation<>EXCLUDED.installation_generation OR environment_vault_layers.host_key_generation<>EXCLUDED.host_key_generation THEN NULL ELSE environment_vault_layers.observation END`, machine, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, claims.DeliveryGeneration, digestText(id[:]), claims.InstallationGeneration, claims.HostKeyGeneration, claims.FenceGeneration, account, claims.WriterPublic, raw)
		if err != nil {
			return err
		}
		out, err = readVaultLayerTx(ctx, tx, machine, c)
		if err != nil {
			return err
		}
		return saveVaultOperationTx(ctx, tx, account, in.OperationID, digest, out)
	})
	return out, err
}

func (s *Service) VaultLayerRecipients(ctx context.Context, account string, source VaultLayerCoordinate, state string, limit, offset int) ([]VaultLayerRecipient, int, error) {
	items := []VaultLayerRecipient{}
	total := 0
	if limit < 1 || limit > 200 || offset < 0 || offset > 2147483647 || state != "pending" && state != "ready" && state != "" {
		return items, 0, ErrProtocolInvalid
	}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		writable, err := vaultLayerWriterTx(ctx, tx, account, source)
		if err != nil {
			return err
		}
		if _, err := readVaultScopeWorkspaceTx(ctx, tx, source.WorkspaceID, source.OwnerKind, source.OwnerID, source.MachineID); err != nil {
			return err
		}
		const from = ` FROM user_machines m JOIN users u ON u.id=m.user_id AND u.status='active' JOIN environment_vault_hosts h ON h.machine_id=m.id AND h.account_id=m.user_id AND h.installation_generation=m.installation_generation JOIN environment_vault_layer_fences f ON f.workspace_id=$2 AND f.owner_kind=$3 AND f.owner_id=$4 AND f.source_machine_id=$5 LEFT JOIN environment_vault_layers l ON l.machine_id=m.id AND l.workspace_id=f.workspace_id AND l.owner_kind=f.owner_kind AND l.owner_id=f.owner_id AND l.source_machine_id=f.source_machine_id WHERE m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.state NOT IN('deleted','revoked') AND 'environment_injection'=ANY(m.configured_capabilities) AND (($3='personal' AND m.user_id=$4 AND ($5='' OR m.id=$5)) OR ($3='team' AND ($6 OR m.user_id=$1) AND EXISTS(SELECT 1 FROM teams t JOIN team_members member USING(team_id) WHERE t.team_id=$2 AND t.deleted_at IS NULL AND NOT t.suspended AND team_paid_entitled(t.team_id) AND member.account_id=m.user_id AND member.active)))`
		const current = `(coalesce(l.installation_generation,0)=m.installation_generation AND coalesce(l.host_key_generation,0)=h.host_key_generation AND coalesce(l.applied_fence_generation,0)=f.generation AND coalesce(octet_length(l.envelope),0)>0)`
		filter := ` AND ($7='' OR ($7='ready' AND ` + current + `) OR ($7='pending' AND NOT ` + current + `))`
		args := []any{account, source.WorkspaceID, source.OwnerKind, source.OwnerID, source.MachineID, writable, state}
		if err := tx.QueryRow(ctx, `SELECT count(*)`+from+filter, args...).Scan(&total); err != nil {
			return err
		}
		args = append(args, limit, offset)
		rows, err := tx.Query(ctx, `SELECT m.user_id,m.id,m.installation_generation,h.host_key_generation,h.host_public,coalesce(l.delivery_generation,0),coalesce(l.document_id,''),f.generation`+from+filter+` ORDER BY m.id LIMIT $8 OFFSET $9`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r VaultLayerRecipient
			var public []byte
			if err := rows.Scan(&r.RecipientAccount, &r.MachineID, &r.InstallationGeneration, &r.HostKeyGeneration, &public, &r.DeliveryGeneration, &r.DocumentID, &r.FenceGeneration); err != nil {
				return err
			}
			r.HostPublic = b64VaultPublic(public)
			items = append(items, r)
		}
		return rows.Err()
	})
	return items, total, err
}

func (s *Service) VaultLayerContext(ctx context.Context, machineOwner, machine, actor, workspace string) (VaultLayerContext, error) {
	out := VaultLayerContext{WorkspaceID: workspace, ActorAccountID: actor, MachineID: machine, Layers: []VaultLayerDelivery{}}
	if !validIdentifier(actor) || !validIdentifier(workspace) {
		return out, ErrInvalidScope
	}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		if _, err := authorizeVaultHostTx(ctx, tx, machineOwner, machine); err != nil {
			return err
		}
		if actor == machineOwner {
			configured, err := vaultAccountConfigurationGenerationTx(ctx, tx, machineOwner)
			if err != nil {
				return err
			}
			if configured > 0 {
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environment_password_vaults WHERE account_id=$1)`, machineOwner).Scan(&exists); err != nil {
					return err
				}
				if !exists {
					return ErrNotFound
				}
			}
		}
		var public []byte
		if err := tx.QueryRow(ctx, `SELECT h.account_id,h.installation_generation,h.host_key_generation,h.host_public FROM environment_vault_hosts h JOIN user_machines m ON m.id=h.machine_id AND m.installation_generation=h.installation_generation JOIN users actor ON actor.id=$3 AND actor.status='active' WHERE h.machine_id=$1 AND h.account_id=$2`, machine, machineOwner, actor).Scan(&out.Recipient.RecipientAccount, &out.Recipient.InstallationGeneration, &out.Recipient.HostKeyGeneration, &public); errors.Is(err, pgx.ErrNoRows) {
			return ErrKeyAuthorizationRequired
		} else if err != nil {
			return err
		}
		out.Recipient.MachineID = machine
		out.Recipient.HostPublic = b64VaultPublic(public)
		coordinates := []VaultLayerCoordinate{}
		if workspace == "personal" {
			if actor != machineOwner {
				return ErrKeyAuthorizationRequired
			}
		} else {
			if err := requireVaultWorkspaceMemberTx(ctx, tx, actor, workspace); err != nil {
				return err
			}
			if err := requireVaultWorkspaceMemberTx(ctx, tx, machineOwner, workspace); err != nil {
				return err
			}
			var rotation bool
			err := tx.QueryRow(ctx, `SELECT rotation_required FROM environment_vault_teams WHERE team_id=$1`, workspace).Scan(&rotation)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if rotation {
				return ErrVaultRotationRequired
			}
			coordinates = append(coordinates, VaultLayerCoordinate{WorkspaceID: workspace, OwnerKind: "team", OwnerID: workspace})
		}
		if actor == machineOwner {
			coordinates = append(coordinates, VaultLayerCoordinate{WorkspaceID: workspace, OwnerKind: "personal", OwnerID: actor}, VaultLayerCoordinate{WorkspaceID: workspace, OwnerKind: "personal", OwnerID: actor, MachineID: machine})
		}
		for _, c := range coordinates {
			scope, err := readVaultScopeWorkspaceTx(ctx, tx, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			// Anchors and deleted record histories assign no ENV. Determine the
			// current epoch's live set under the same scope lock and transaction;
			// nonempty scopes still require the exact recipient key publication.
			var assigned bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM(SELECT DISTINCT ON(record_id) deleted FROM environment_vault_record_versions WHERE `+recordScopePredicate+` ORDER BY record_id,revision DESC) current WHERE NOT deleted)`, recordArgs(c, scope.KeyEpoch)...).Scan(&assigned); err != nil {
				return err
			}
			if !assigned {
				continue
			}
			layer, err := readVaultLayerTx(ctx, tx, machine, c)
			if err != nil {
				return err
			}
			out.Layers = append(out.Layers, layer)
		}
		return nil
	})
	return out, err
}
