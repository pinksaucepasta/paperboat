package environmentmanager

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

var ErrVaultTeamGrantRequired = errors.New("ENV team key grant is required")

type VaultDataClient interface {
	GetVaultScope(context.Context, string, string, string) (api.VaultScopeState, error)
	PutVaultScope(context.Context, string, string, string, api.VaultScopePut) (api.VaultScopeState, error)
	GetVaultSharing(context.Context, string) (api.VaultSharingState, error)
	GetVaultTeam(context.Context, string) (api.VaultTeamState, error)
	CreateVaultTeam(context.Context, api.VaultTeamCreate) (api.VaultTeamState, error)
	GrantVaultTeamMember(context.Context, string, api.VaultMemberGrant) (api.VaultTeamState, error)
	RotateVaultTeam(context.Context, string, api.VaultTeamRotate) (api.VaultTeamState, error)
	GetVaultGrants(context.Context) ([]api.VaultGrantState, error)
	AckVaultGrant(context.Context, string, string) error
}

func (v PasswordVault) dataClient() (VaultDataClient, error) {
	c, ok := v.Client.(VaultDataClient)
	if !ok {
		return nil, environmente2ee.ErrInvalid
	}
	return c, nil
}
func newVaultOperationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "envop_" + hex.EncodeToString(b[:]), nil
}
func vaultEncoded(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }
func (v PasswordVault) withVaultKeys(ctx context.Context, fn func(*config.PasswordVaultRecord, *environmente2ee.VaultKeys, VaultDataClient) error) (resultErr error) {
	unlock, err := v.lock()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	c, err := v.dataClient()
	if err != nil {
		return err
	}
	local, err := v.Store.LoadPasswordVault(v.Issuer, v.AccountID)
	if err != nil {
		return err
	}
	defer local.Clear()
	if local.Pending != nil || local.Operation != nil {
		return ErrVaultPending
	}
	if len(local.Payload) == 0 {
		return ErrVaultLocked
	}
	head, _, err := v.current(ctx)
	if err != nil {
		return err
	}
	if head != local.Head {
		return ErrVaultChanged
	}
	keys, err := environmente2ee.ParseVaultKeys(local.Payload)
	if err != nil {
		return ErrIntegrity
	}
	defer keys.Clear()
	return fn(&local, &keys, c)
}
func scopeKey(keys *environmente2ee.VaultKeys, kind, owner, account string) ([]byte, uint64, error) {
	if kind == "personal" && owner == account {
		return keys.PersonalKey, keys.PersonalEpoch, nil
	}
	if kind == "team" {
		for _, t := range keys.Teams {
			if t.TeamID == owner {
				return t.Key, t.Epoch, nil
			}
		}
		return nil, 0, ErrVaultTeamGrantRequired
	}
	return nil, 0, ErrIntegrity
}
func (v PasswordVault) readVaultScope(ctx context.Context, c VaultDataClient, keys *environmente2ee.VaultKeys, kind, owner, machine string) (environmente2ee.VaultScope, map[string][]byte, error) {
	if kind == "team" {
		team, err := c.GetVaultTeam(ctx, owner)
		if err != nil {
			return environmente2ee.VaultScope{}, nil, err
		}
		if _, ok := vaultTeamActor(team, v.AccountID, "read"); !ok {
			return environmente2ee.VaultScope{}, nil, ErrIntegrity
		}
	}
	key, epoch, err := scopeKey(keys, kind, owner, v.AccountID)
	if err != nil {
		return environmente2ee.VaultScope{}, nil, err
	}
	state, err := c.GetVaultScope(ctx, kind, owner, machine)
	if err != nil {
		return environmente2ee.VaultScope{}, nil, err
	}
	scope, err := state.Decode()
	if err != nil || scope.Claims.Issuer != v.Issuer || scope.Claims.OwnerKind != kind || scope.Claims.OwnerID != owner || scope.Claims.MachineID != machine || scope.Claims.KeyEpoch != epoch {
		return environmente2ee.VaultScope{}, nil, ErrIntegrity
	}
	values, err := environmente2ee.OpenVaultScope(ctx, scope, key)
	return scope, values, err
}
func clearVaultValues(values map[string][]byte) {
	for _, value := range values {
		clear(value)
	}
}

// MutateScope preserves write-only CLI input: plaintext exists only inside this
// callback and local encryption, never in the API request or output metadata.
func (v PasswordVault) MutateScope(ctx context.Context, kind, owner, machine string, mutate func(map[string][]byte) error) error {
	if mutate == nil {
		return environmente2ee.ErrInvalid
	}
	return v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *environmente2ee.VaultKeys, c VaultDataClient) error {
		if kind == "team" {
			team, err := c.GetVaultTeam(ctx, owner)
			if err != nil {
				return err
			}
			if _, ok := vaultTeamActor(team, v.AccountID, "write"); !ok {
				return ErrIntegrity
			}
		}
		old, values, err := v.readVaultScope(ctx, c, keys, kind, owner, machine)
		if api.IsNotFound(err) {
			values = map[string][]byte{}
		} else if err != nil {
			return err
		}
		defer clearVaultValues(values)
		if err := mutate(values); err != nil {
			return err
		}
		key, epoch, err := scopeKey(keys, kind, owner, v.AccountID)
		if err != nil {
			return err
		}
		claims := environmente2ee.VaultScopeClaims{Issuer: v.Issuer, OwnerKind: kind, OwnerID: owner, MachineID: machine, KeyEpoch: epoch, Revision: old.Claims.Revision + 1, Previous: old.ID[:], WriterAccount: v.AccountID, WriterVaultGeneration: local.Head.Generation}
		next, err := environmente2ee.SealVaultScope(ctx, claims, key, keys.WriterSeed, values)
		if err != nil {
			return err
		}
		op, err := newVaultOperationID()
		if err != nil {
			return err
		}
		return v.stageOperation(ctx, local, "scope-put", kind, owner, machine, api.VaultScopePut{OperationID: op, Envelope: vaultEncoded(next.Raw)})
	})
}
func (v PasswordVault) stageOperation(ctx context.Context, local *config.PasswordVaultRecord, kind, ownerKind, owner, machine string, request any) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	local.Operation = &config.VaultOperation{Kind: kind, OwnerKind: ownerKind, OwnerID: owner, MachineID: machine, Request: raw}
	if err := v.Store.SavePasswordVault(*local); err != nil {
		return err
	}
	return v.publishOperation(ctx, local)
}
func (v PasswordVault) publishOperation(ctx context.Context, local *config.PasswordVaultRecord) error {
	c, err := v.dataClient()
	if err != nil {
		return err
	}
	op := local.Operation
	if op == nil {
		return environmente2ee.ErrInvalid
	}
	switch op.Kind {
	case "scope-put":
		var in api.VaultScopePut
		if json.Unmarshal(op.Request, &in) != nil {
			return ErrIntegrity
		}
		out, err := c.PutVaultScope(ctx, op.OwnerKind, op.OwnerID, op.MachineID, in)
		if err != nil {
			return err
		}
		if out.Envelope != in.Envelope {
			return ErrIntegrity
		}
	case "team-create":
		var in api.VaultTeamCreate
		if json.Unmarshal(op.Request, &in) != nil {
			return ErrIntegrity
		}
		out, err := c.CreateVaultTeam(ctx, in)
		if err != nil {
			return err
		}
		if out.TeamID != in.TeamID || out.Scope.Envelope != in.ScopeEnvelope {
			return ErrIntegrity
		}
	case "team-grant":
		var in api.VaultMemberGrant
		if json.Unmarshal(op.Request, &in) != nil {
			return ErrIntegrity
		}
		out, err := c.GrantVaultTeamMember(ctx, op.OwnerID, in)
		if err != nil {
			return err
		}
		if out.TeamID != op.OwnerID {
			return ErrIntegrity
		}
	case "team-rotate":
		var in api.VaultTeamRotate
		if json.Unmarshal(op.Request, &in) != nil {
			return ErrIntegrity
		}
		out, err := c.RotateVaultTeam(ctx, op.OwnerID, in)
		if err != nil {
			return err
		}
		if out.TeamID != op.OwnerID || out.Scope.Envelope != in.ScopeEnvelope {
			return ErrIntegrity
		}
	case "personal-rotate":
		if err := v.publishPersonalRotation(ctx, local); err != nil {
			return err
		}
	case "host-provision":
		hostClient, ok := v.Client.(VaultHostClient)
		if !ok {
			return environmente2ee.ErrInvalid
		}
		var in api.VaultHostProvision
		if json.Unmarshal(op.Request, &in) != nil {
			return ErrIntegrity
		}
		out, err := hostClient.ProvisionVaultHost(ctx, op.MachineID, in)
		if err != nil {
			return err
		}
		if out.Envelope != in.Envelope || out.MachineID != op.MachineID || out.AccountID != v.AccountID || out.State != "ready" {
			return ErrIntegrity
		}
	case "reset":
		resetClient, ok := v.Client.(VaultResetClient)
		if !ok {
			return environmente2ee.ErrInvalid
		}
		var in api.VaultReset
		if json.Unmarshal(op.Request, &in) != nil {
			return ErrIntegrity
		}
		out, err := resetClient.ResetVault(ctx, in)
		if err != nil {
			return err
		}
		if out.Envelope != in.VaultEnvelope {
			return ErrIntegrity
		}
	default:
		return environmente2ee.ErrInvalid
	}
	if local.Pending != nil {
		head, _, err := v.current(ctx)
		if err != nil {
			return err
		}
		if head != local.Pending.Head {
			return ErrVaultChanged
		}
		next := config.PasswordVaultRecord{Head: head, Envelope: local.Pending.Envelope, Payload: bytes.Clone(local.Pending.Payload)}
		defer next.Clear()
		if err := v.Store.SavePasswordVault(next); err != nil {
			return err
		}
		local.Clear()
		return nil
	}
	local.Operation = nil
	return v.Store.SavePasswordVault(*local)
}
func (v PasswordVault) prepareKeySuccessor(ctx context.Context, local *config.PasswordVaultRecord, keys *environmente2ee.VaultKeys) error {
	payload, err := keys.MarshalBinary()
	if err != nil {
		return err
	}
	protection, err := environmente2ee.PasswordVaultProtection(local.Envelope)
	if err != nil {
		clear(payload)
		return err
	}
	raw, head, err := environmente2ee.SealProtectedVault(ctx, environmente2ee.VaultHead{Issuer: v.Issuer, AccountID: v.AccountID, Generation: local.Head.Generation + 1}, local.Head.ID, protection, payload)
	if err != nil {
		clear(payload)
		return err
	}
	local.Pending = &config.PasswordVaultPending{Head: head, Envelope: raw, Payload: payload}
	return nil
}
func replaceTeamKey(keys *environmente2ee.VaultKeys, team environmente2ee.VaultTeamKey) error {
	for i := range keys.Teams {
		if keys.Teams[i].TeamID == team.TeamID {
			if keys.Teams[i].Epoch > team.Epoch || keys.Teams[i].MembershipGeneration > team.MembershipGeneration {
				return ErrAuthorityFork
			}
			clear(keys.Teams[i].Key)
			keys.Teams[i] = team
			return nil
		}
	}
	keys.Teams = append(keys.Teams, team)
	sort.Slice(keys.Teams, func(i, j int) bool { return keys.Teams[i].TeamID < keys.Teams[j].TeamID })
	return nil
}
func (v PasswordVault) CreateTeam(ctx context.Context, teamID string) error {
	return v.CreateTeamAt(ctx, teamID, 0, 1)
}
func (v PasswordVault) CreateTeamAt(ctx context.Context, teamID string, expectedTeamGeneration, membershipGeneration uint64) error {
	return v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *environmente2ee.VaultKeys, c VaultDataClient) error {
		if membershipGeneration == 0 {
			return ErrIntegrity
		}
		for _, team := range keys.Teams {
			if team.TeamID == teamID {
				return ErrVaultChanged
			}
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		defer clear(key)
		op, err := newVaultOperationID()
		if err != nil {
			return err
		}
		scope, err := environmente2ee.SealVaultScope(ctx, environmente2ee.VaultScopeClaims{Issuer: v.Issuer, OwnerKind: "team", OwnerID: teamID, KeyEpoch: 1, Revision: 1, Previous: make([]byte, 32), WriterAccount: v.AccountID, WriterVaultGeneration: local.Head.Generation}, key, keys.WriterSeed, map[string][]byte{})
		if err != nil {
			return err
		}
		sharing, err := ecdh.X25519().NewPrivateKey(keys.SharingPrivate)
		if err != nil {
			return err
		}
		grant, err := environmente2ee.SealTeamGrant(ctx, environmente2ee.TeamGrantClaims{Issuer: v.Issuer, TeamID: teamID, TeamEpoch: 1, MembershipGeneration: membershipGeneration, SenderAccount: v.AccountID, RecipientAccount: v.AccountID, RecipientVaultGeneration: local.Head.Generation + 1, RecipientSharingPublic: sharing.PublicKey().Bytes(), OperationID: op}, key, keys.WriterSeed)
		if err != nil {
			return err
		}
		if err := replaceTeamKey(keys, environmente2ee.VaultTeamKey{TeamID: teamID, Epoch: 1, MembershipGeneration: membershipGeneration, Key: bytes.Clone(key)}); err != nil {
			return err
		}
		if err := v.prepareKeySuccessor(ctx, local, keys); err != nil {
			return err
		}
		return v.stageOperation(ctx, local, "team-create", "team", teamID, "", api.VaultTeamCreate{OperationID: op, TeamID: teamID, ScopeEnvelope: vaultEncoded(scope.Raw), GrantEnvelope: vaultEncoded(grant.Raw), VaultEnvelope: vaultEncoded(local.Pending.Envelope), ExpectedTeamGeneration: expectedTeamGeneration})
	})
}
func (v PasswordVault) GrantTeam(ctx context.Context, teamID, accountID string) error {
	return v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *environmente2ee.VaultKeys, c VaultDataClient) error {
		team, err := c.GetVaultTeam(ctx, teamID)
		if err != nil {
			return err
		}
		if team.TeamID != teamID {
			return ErrIntegrity
		}
		actor, ok := vaultTeamActor(team, v.AccountID, "read")
		if !ok || (actor.Role != "owner" && actor.Role != "admin") {
			return ErrIntegrity
		}
		key, epoch, err := scopeKey(keys, "team", teamID, v.AccountID)
		if err != nil {
			return err
		}
		if epoch != team.KeyEpoch {
			return ErrVaultChanged
		}
		var previous uint64
		eligible := false
		for _, member := range team.Members {
			if member.AccountID == accountID {
				previous = member.MembershipGeneration
				eligible = member.Active && (member.EnvPermission == "read" || member.EnvPermission == "write")
			}
		}
		if !eligible || previous == 0 {
			return ErrVaultChanged
		}
		membership := previous
		recipient, err := c.GetVaultSharing(ctx, accountID)
		if err != nil {
			return err
		}
		if recipient.AccountID != accountID {
			return ErrIntegrity
		}
		public, err := base64.RawURLEncoding.Strict().DecodeString(recipient.SharingPublic)
		if err != nil {
			return ErrIntegrity
		}
		op, err := newVaultOperationID()
		if err != nil {
			return err
		}
		grant, err := environmente2ee.SealTeamGrant(ctx, environmente2ee.TeamGrantClaims{Issuer: v.Issuer, TeamID: teamID, TeamEpoch: epoch, MembershipGeneration: membership, SenderAccount: v.AccountID, RecipientAccount: accountID, RecipientVaultGeneration: recipient.VaultGeneration, RecipientSharingPublic: public, OperationID: op}, key, keys.WriterSeed)
		if err != nil {
			return err
		}
		return v.stageOperation(ctx, local, "team-grant", "team", teamID, "", api.VaultMemberGrant{OperationID: op, AccountID: accountID, ExpectedTeamGeneration: team.Generation, ExpectedMembershipGeneration: previous, GrantEnvelope: vaultEncoded(grant.Raw)})
	})
}

// SyncTeamGrants commits each new key to the current unified vault before ack.
// Both password and recovery paths therefore see exactly the same new team keys.
func (v PasswordVault) SyncTeamGrants(ctx context.Context) error {
	c, err := v.dataClient()
	if err != nil {
		return err
	}
	grants, err := c.GetVaultGrants(ctx)
	if err != nil {
		return err
	}
	if len(grants) > environmente2ee.MaximumVaultTeams {
		return ErrIntegrity
	}
	for _, item := range grants {
		err = v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *environmente2ee.VaultKeys, c VaultDataClient) error {
			if len(item.Envelope) > base64.RawURLEncoding.EncodedLen(4096) {
				return ErrIntegrity
			}
			raw, err := base64.RawURLEncoding.Strict().DecodeString(item.Envelope)
			if err != nil {
				return ErrIntegrity
			}
			writer, err := base64.RawURLEncoding.Strict().DecodeString(item.SenderWriterPublic)
			if err != nil {
				return ErrIntegrity
			}
			grant, err := environmente2ee.ParseTeamGrant(raw, writer)
			if err != nil || grant.ID.String() != item.DocumentID || grant.Claims.TeamID != item.TeamID {
				return ErrIntegrity
			}
			team, err := c.GetVaultTeam(ctx, item.TeamID)
			if err != nil {
				return err
			}
			if team.KeyEpoch != item.TeamEpoch {
				return ErrVaultChanged
			}
			authorized := false
			for _, m := range team.Members {
				if m.AccountID == v.AccountID && m.Active && (m.EnvPermission == "read" || m.EnvPermission == "write") && m.MembershipGeneration == item.MembershipGeneration && m.GrantEpoch == item.TeamEpoch {
					authorized = true
				}
			}
			if !authorized {
				return ErrIntegrity
			}
			key, err := environmente2ee.OpenTeamGrant(ctx, grant, v.Issuer, v.AccountID, local.Head.Generation, item.TeamEpoch, item.MembershipGeneration, keys.SharingPrivate)
			if err != nil {
				return err
			}
			defer clear(key)
			same := false
			for _, existing := range keys.Teams {
				if existing.TeamID == item.TeamID && existing.Epoch == item.TeamEpoch && existing.MembershipGeneration == item.MembershipGeneration && bytes.Equal(existing.Key, key) {
					same = true
				}
			}
			if !same {
				if err := replaceTeamKey(keys, environmente2ee.VaultTeamKey{TeamID: item.TeamID, Epoch: item.TeamEpoch, MembershipGeneration: item.MembershipGeneration, Key: bytes.Clone(key)}); err != nil {
					return err
				}
				if err := v.prepareKeySuccessor(ctx, local, keys); err != nil {
					return err
				}
				if err := v.Store.SavePasswordVault(*local); err != nil {
					return err
				}
				if err := v.publish(ctx, local); err != nil {
					return err
				}
			}
			return c.AckVaultGrant(ctx, item.DocumentID, local.Head.ID.String())
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// RotateTeam atomically replaces the epoch, all remaining individual grants,
// scope ciphertext and the initiating owner's vault. Disclosed old keys remain
// usable only for previously copied ciphertext. Total loss is explicit.
func (v PasswordVault) RotateTeam(ctx context.Context, teamID string, remove []string, totalLoss bool) error {
	return v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *environmente2ee.VaultKeys, c VaultDataClient) error {
		team, err := c.GetVaultTeam(ctx, teamID)
		if err != nil {
			return err
		}
		actor, actorOK := vaultTeamActor(team, v.AccountID, "read")
		if totalLoss {
			actorOK = actor.Active && actor.Role == "owner"
		}
		if team.TeamID != teamID || !actorOK || (actor.Role != "owner" && actor.Role != "admin") {
			return ErrIntegrity
		}
		var previous environmente2ee.VaultScope
		previousID, err := environmente2ee.ParseDocumentID(team.Scope.DocumentID)
		if err != nil || team.Scope.OwnerKind != "team" || team.Scope.OwnerID != teamID || team.Scope.KeyEpoch != team.KeyEpoch || team.Scope.Revision == 0 {
			return ErrIntegrity
		}
		values := map[string][]byte{}
		if !totalLoss {
			previous, err = team.Scope.Decode()
			if err != nil || previous.Claims.Issuer != v.Issuer || previous.Claims.OwnerKind != "team" || previous.Claims.OwnerID != teamID || previous.Claims.KeyEpoch != team.KeyEpoch || previous.ID != previousID {
				return ErrIntegrity
			}
			oldKey, epoch, err := scopeKey(keys, "team", teamID, v.AccountID)
			if err != nil {
				return err
			}
			if epoch != team.KeyEpoch {
				return ErrVaultChanged
			}
			values, err = environmente2ee.OpenVaultScope(ctx, previous, oldKey)
			if err != nil {
				return err
			}
		}
		defer clearVaultValues(values)
		removed := make(map[string]bool, len(remove))
		for _, account := range remove {
			if account == v.AccountID || removed[account] {
				return environmente2ee.ErrInvalid
			}
			removed[account] = true
		}
		if !validVaultTeamRemovals(team.Members, actor.Role, removed) {
			return ErrVaultChanged
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		defer clear(key)
		op, err := newVaultOperationID()
		if err != nil {
			return err
		}
		scope, err := environmente2ee.SealVaultScope(ctx, environmente2ee.VaultScopeClaims{Issuer: v.Issuer, OwnerKind: "team", OwnerID: teamID, KeyEpoch: team.KeyEpoch + 1, Revision: team.Scope.Revision + 1, Previous: previousID[:], WriterAccount: v.AccountID, WriterVaultGeneration: local.Head.Generation}, key, keys.WriterSeed, values)
		if err != nil {
			return err
		}
		grants := make([]string, 0, len(team.Members))
		var ownMembership uint64
		for _, member := range team.Members {
			if !member.Active || removed[member.AccountID] || (member.EnvPermission != "read" && member.EnvPermission != "write") {
				continue
			}
			recipient, err := c.GetVaultSharing(ctx, member.AccountID)
			if err != nil {
				return err
			}
			if recipient.AccountID != member.AccountID {
				return ErrIntegrity
			}
			public, err := base64.RawURLEncoding.Strict().DecodeString(recipient.SharingPublic)
			if err != nil {
				return ErrIntegrity
			}
			if member.AccountID == v.AccountID {
				if recipient.VaultGeneration != local.Head.Generation {
					return ErrVaultChanged
				}
				recipient.VaultGeneration++
				ownMembership = member.MembershipGeneration
			}
			grant, err := environmente2ee.SealTeamGrant(ctx, environmente2ee.TeamGrantClaims{Issuer: v.Issuer, TeamID: teamID, TeamEpoch: team.KeyEpoch + 1, MembershipGeneration: member.MembershipGeneration, SenderAccount: v.AccountID, RecipientAccount: member.AccountID, RecipientVaultGeneration: recipient.VaultGeneration, RecipientSharingPublic: public, OperationID: op}, key, keys.WriterSeed)
			if err != nil {
				return err
			}
			grants = append(grants, vaultEncoded(grant.Raw))
		}
		if ownMembership == 0 {
			return ErrIntegrity
		}
		if err := replaceTeamKey(keys, environmente2ee.VaultTeamKey{TeamID: teamID, Epoch: team.KeyEpoch + 1, MembershipGeneration: ownMembership, Key: bytes.Clone(key)}); err != nil {
			return err
		}
		if err := v.prepareKeySuccessor(ctx, local, keys); err != nil {
			return err
		}
		return v.stageOperation(ctx, local, "team-rotate", "team", teamID, "", api.VaultTeamRotate{OperationID: op, ExpectedTeamGeneration: team.Generation, RemoveAccountIDs: append([]string{}, remove...), ScopeEnvelope: vaultEncoded(scope.Raw), GrantEnvelopes: grants, VaultEnvelope: vaultEncoded(local.Pending.Envelope), ConfirmTotalLoss: totalLoss})
	})
}

func vaultTeamActor(team api.VaultTeamState, account, permission string) (api.VaultTeamMember, bool) {
	for _, member := range team.Members {
		if member.AccountID != account || !member.Active {
			continue
		}
		allowed := member.EnvPermission == "write" || (permission == "read" && member.EnvPermission == "read")
		return member, allowed
	}
	return api.VaultTeamMember{}, false
}

func validVaultTeamRemovals(members []api.VaultTeamMember, actorRole string, removed map[string]bool) bool {
	found := 0
	for _, member := range members {
		if !removed[member.AccountID] {
			continue
		}
		if actorRole == "admin" && member.Role != "member" {
			return false
		}
		found++
	}
	return found == len(removed)
}

// ListScopeNames returns only configured names; values stay inside local custody.
func (v PasswordVault) ListScopeNames(ctx context.Context, kind, owner, machine string) ([]string, error) {
	var names []string
	err := v.withVaultKeys(ctx, func(_ *config.PasswordVaultRecord, keys *environmente2ee.VaultKeys, c VaultDataClient) error {
		_, values, err := v.readVaultScope(ctx, c, keys, kind, owner, machine)
		if api.IsNotFound(err) {
			names = []string{}
			return nil
		}
		if err != nil {
			return err
		}
		defer clearVaultValues(values)
		names = make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil
	})
	return names, err
}
