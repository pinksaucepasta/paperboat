package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"sort"

	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type teamMember struct {
	Account    string `json:"account_id"`
	Membership uint64 `json:"membership_generation"`
	Role       string `json:"role"`
	Active     bool   `json:"active"`
	GrantEpoch uint64 `json:"grant_epoch"`
	Permission string `json:"env_permission"`
}
type teamState struct {
	ID               string       `json:"team_id"`
	Owner            string       `json:"owner_account"`
	Generation       uint64       `json:"generation"`
	Epoch            uint64       `json:"key_epoch"`
	Members          []teamMember `json:"members"`
	Scope            scopeState   `json:"scope"`
	RotationRequired bool         `json:"rotation_required"`
}
type sharingState struct {
	Account    string `json:"account_id"`
	Generation uint64 `json:"vault_generation"`
	Public     string `json:"sharing_public"`
}
type grantState struct {
	Team       string `json:"team_id"`
	Membership uint64 `json:"membership_generation"`
	Epoch      uint64 `json:"team_epoch"`
	Document   string `json:"document_id"`
	Envelope   string `json:"envelope"`
	Writer     string `json:"sender_writer_public"`
}

func (t teamState) actor(account, permission string) (teamMember, bool) {
	for _, m := range t.Members {
		if m.Account == account {
			return m, m.Active && (m.Permission == "write" || (permission == "read" && m.Permission == "read"))
		}
	}
	return teamMember{}, false
}
func cloneKeys(keys env.VaultKeys) (env.VaultKeys, error) {
	raw, err := keys.MarshalBinary()
	if err != nil {
		return env.VaultKeys{}, err
	}
	defer clear(raw)
	return env.ParseVaultKeys(raw)
}
func replaceTeam(keys *env.VaultKeys, team env.VaultTeamKey) error {
	for i, t := range keys.Teams {
		if t.TeamID == team.TeamID {
			if t.Epoch > team.Epoch || t.MembershipGeneration > team.MembershipGeneration {
				return env.ErrInvalid
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
func (e *engine) successor(ctx context.Context, keys env.VaultKeys) (string, error) {
	protection, err := env.PasswordVaultProtection(e.raw)
	if err != nil {
		return "", err
	}
	h := e.head
	h.Generation++
	h.ID = env.DocumentID{}
	out, err := e.stage(ctx, h, e.head.ID, protection, keys)
	if err != nil {
		return "", err
	}
	e.pendingKeys = &keys
	return out.(map[string]any)["envelope"].(string), nil
}
func (e *engine) teamKey(t teamState, permission string) ([]byte, teamMember, error) {
	actor, ok := t.actor(e.head.AccountID, permission)
	if !ok || t.ID == "" || t.Epoch == 0 {
		return nil, actor, errors.New("Current Team ENV membership and permission are required.")
	}
	for _, key := range e.keys.Teams {
		if key.TeamID == t.ID && key.Epoch == t.Epoch && key.MembershipGeneration == actor.Membership {
			return key.Key, actor, nil
		}
	}
	return nil, actor, errors.New("Accept the current Team ENV key grant first.")
}
func scopeDocument(state scopeState) (env.VaultScope, error) {
	raw, err := decode(state.Envelope)
	if err != nil {
		return env.VaultScope{}, err
	}
	writer, err := decode(state.WriterPublic)
	if err != nil {
		return env.VaultScope{}, err
	}
	scope, err := env.ParseVaultScope(raw, writer)
	if err != nil {
		return scope, err
	}
	c := scope.Claims
	if c.WorkspaceID != state.WorkspaceID || c.OwnerKind != state.Kind || c.OwnerID != state.Owner || c.MachineID != state.Machine || c.KeyEpoch != state.Epoch || c.Revision != state.Revision || digest(scope.ID) != state.DocumentID {
		return scope, env.ErrInvalid
	}
	return scope, nil
}
func (e *engine) teamOperation(ctx context.Context, r request) (any, error) {
	if err := e.current(r.Vault); err != nil {
		return nil, err
	}
	if r.Team == nil || r.Team.ID != r.Owner || r.Operation == "" || len(r.Team.Members) > 128 || len(r.Sharing) > 128 || len(r.Remove) > 128 {
		return nil, env.ErrInvalid
	}
	t := *r.Team
	switch r.Action {
	case "team-create":
		if t.Owner != e.head.AccountID || t.Epoch != 0 || r.Membership == 0 {
			return nil, env.ErrInvalid
		}
		keys, err := cloneKeys(e.keys)
		if err != nil {
			return nil, err
		}
		owned := true
		defer func() {
			if owned {
				keys.Clear()
			}
		}()
		for _, key := range keys.Teams {
			if key.TeamID == t.ID {
				return nil, env.ErrInvalid
			}
		}
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		defer clear(key)
		scope, err := env.SealVaultScope(ctx, env.VaultScopeClaims{Issuer: e.head.Issuer, OwnerKind: "team", OwnerID: t.ID, WorkspaceID: t.ID, KeyEpoch: 1, Revision: 1, Previous: make([]byte, 32), WriterAccount: e.head.AccountID, WriterVaultGeneration: e.head.Generation}, key, keys.WriterSeed, map[string][]byte{})
		if err != nil {
			return nil, err
		}
		sharing, err := ecdh.X25519().NewPrivateKey(keys.SharingPrivate)
		if err != nil {
			return nil, err
		}
		grant, err := env.SealTeamGrant(ctx, env.TeamGrantClaims{Issuer: e.head.Issuer, TeamID: t.ID, TeamEpoch: 1, MembershipGeneration: r.Membership, SenderAccount: e.head.AccountID, RecipientAccount: e.head.AccountID, RecipientVaultGeneration: e.head.Generation + 1, RecipientSharingPublic: sharing.PublicKey().Bytes(), OperationID: r.Operation}, key, keys.WriterSeed)
		if err != nil {
			return nil, err
		}
		if err = replaceTeam(&keys, env.VaultTeamKey{TeamID: t.ID, Epoch: 1, MembershipGeneration: r.Membership, Key: bytes.Clone(key)}); err != nil {
			return nil, err
		}
		vault, err := e.successor(ctx, keys)
		if err != nil {
			return nil, err
		}
		owned = false
		return map[string]any{"operation_id": r.Operation, "team_id": t.ID, "scope_envelope": encoded(scope.Raw), "grant_envelope": encoded(grant.Raw), "vault_envelope": vault, "expected_team_generation": t.Generation}, nil
	case "team-grant":
		key, actor, err := e.teamKey(t, "read")
		if err != nil {
			return nil, err
		}
		if actor.Role != "owner" && actor.Role != "admin" {
			return nil, env.ErrInvalid
		}
		recipient, ok := t.actor(r.Recipient, "read")
		if !ok || recipient.Membership == 0 || len(r.Sharing) != 1 || r.Sharing[0].Account != r.Recipient {
			return nil, env.ErrInvalid
		}
		target := r.Sharing[0]
		public, err := decode(target.Public)
		if err != nil {
			return nil, err
		}
		grant, err := env.SealTeamGrant(ctx, env.TeamGrantClaims{Issuer: e.head.Issuer, TeamID: t.ID, TeamEpoch: t.Epoch, MembershipGeneration: recipient.Membership, SenderAccount: e.head.AccountID, RecipientAccount: target.Account, RecipientVaultGeneration: target.Generation, RecipientSharingPublic: public, OperationID: r.Operation}, key, e.keys.WriterSeed)
		if err != nil {
			return nil, err
		}
		return map[string]any{"operation_id": r.Operation, "account_id": target.Account, "expected_team_generation": t.Generation, "expected_membership_generation": recipient.Membership, "grant_envelope": encoded(grant.Raw)}, nil
	case "team-accept":
		if r.Grant == nil {
			return nil, env.ErrInvalid
		}
		item := *r.Grant
		member, ok := t.actor(e.head.AccountID, "read")
		if !ok || item.Team != t.ID || item.Epoch != t.Epoch || item.Membership != member.Membership || member.GrantEpoch != item.Epoch {
			return nil, env.ErrInvalid
		}
		raw, err := decode(item.Envelope)
		if err != nil {
			return nil, err
		}
		writer, err := decode(item.Writer)
		if err != nil {
			return nil, err
		}
		grant, err := env.ParseTeamGrant(raw, writer)
		if err != nil || digest(grant.ID) != item.Document || grant.Claims.TeamID != t.ID {
			return nil, env.ErrInvalid
		}
		key, err := env.OpenTeamGrant(ctx, grant, e.head.Issuer, e.head.AccountID, e.head.Generation, item.Epoch, item.Membership, e.keys.SharingPrivate)
		if err != nil {
			return nil, err
		}
		defer clear(key)
		for _, existing := range e.keys.Teams {
			if existing.TeamID == t.ID && existing.Epoch == item.Epoch && existing.MembershipGeneration == item.Membership && bytes.Equal(existing.Key, key) {
				return map[string]any{"already_current": true, "document_id": item.Document}, nil
			}
		}
		keys, err := cloneKeys(e.keys)
		if err != nil {
			return nil, err
		}
		if err = replaceTeam(&keys, env.VaultTeamKey{TeamID: t.ID, Epoch: item.Epoch, MembershipGeneration: item.Membership, Key: bytes.Clone(key)}); err != nil {
			keys.Clear()
			return nil, err
		}
		vault, err := e.successor(ctx, keys)
		if err != nil {
			keys.Clear()
			return nil, err
		}
		return map[string]any{"envelope": vault, "document_id": item.Document}, nil
	case "team-rotate":
		oldKey, actor, err := e.teamKey(t, "read")
		if r.TotalLoss {
			actor, _ = t.actor(e.head.AccountID, "read")
			if actor.Active && actor.Role == "owner" {
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
		if actor.Role != "owner" && actor.Role != "admin" {
			return nil, env.ErrInvalid
		}
		old, err := scopeDocument(t.Scope)
		if err != nil || old.Claims.Issuer != e.head.Issuer || t.Scope.Kind != "team" || t.Scope.Owner != t.ID || t.Scope.Epoch != t.Epoch {
			return nil, env.ErrInvalid
		}
		values := map[string][]byte{}
		if !r.TotalLoss {
			values, err = env.OpenVaultScope(ctx, old, oldKey)
			if err != nil {
				return nil, err
			}
		}
		defer func() {
			for _, v := range values {
				clear(v)
			}
		}()
		removed := map[string]bool{}
		for _, id := range r.Remove {
			if id == e.head.AccountID || removed[id] {
				return nil, env.ErrInvalid
			}
			removed[id] = true
		}
		found := 0
		for _, m := range t.Members {
			if removed[m.Account] {
				if actor.Role == "admin" && m.Role != "member" {
					return nil, env.ErrInvalid
				}
				found++
			}
		}
		if found != len(removed) {
			return nil, env.ErrInvalid
		}
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		defer clear(key)
		scope, err := env.SealVaultScope(ctx, env.VaultScopeClaims{Issuer: e.head.Issuer, OwnerKind: "team", OwnerID: t.ID, WorkspaceID: t.ID, KeyEpoch: t.Epoch + 1, Revision: t.Scope.Revision + 1, Previous: old.ID[:], WriterAccount: e.head.AccountID, WriterVaultGeneration: e.head.Generation}, key, e.keys.WriterSeed, values)
		if err != nil {
			return nil, err
		}
		sharing := map[string]sharingState{}
		for _, s := range r.Sharing {
			if _, exists := sharing[s.Account]; exists {
				return nil, env.ErrInvalid
			}
			sharing[s.Account] = s
		}
		grants := []string{}
		var membership uint64
		for _, m := range t.Members {
			if !m.Active || removed[m.Account] || (m.Permission != "read" && m.Permission != "write") {
				continue
			}
			recipient, ok := sharing[m.Account]
			if !ok {
				return nil, env.ErrInvalid
			}
			public, err := decode(recipient.Public)
			if err != nil {
				return nil, err
			}
			if m.Account == e.head.AccountID {
				if recipient.Generation != e.head.Generation {
					return nil, env.ErrInvalid
				}
				recipient.Generation++
				membership = m.Membership
			}
			grant, err := env.SealTeamGrant(ctx, env.TeamGrantClaims{Issuer: e.head.Issuer, TeamID: t.ID, TeamEpoch: t.Epoch + 1, MembershipGeneration: m.Membership, SenderAccount: e.head.AccountID, RecipientAccount: m.Account, RecipientVaultGeneration: recipient.Generation, RecipientSharingPublic: public, OperationID: r.Operation}, key, e.keys.WriterSeed)
			if err != nil {
				return nil, err
			}
			grants = append(grants, encoded(grant.Raw))
		}
		if membership == 0 {
			return nil, env.ErrInvalid
		}
		keys, err := cloneKeys(e.keys)
		if err != nil {
			return nil, err
		}
		if err = replaceTeam(&keys, env.VaultTeamKey{TeamID: t.ID, Epoch: t.Epoch + 1, MembershipGeneration: membership, Key: bytes.Clone(key)}); err != nil {
			keys.Clear()
			return nil, err
		}
		vault, err := e.successor(ctx, keys)
		if err != nil {
			keys.Clear()
			return nil, err
		}
		return map[string]any{"operation_id": r.Operation, "expected_team_generation": t.Generation, "remove_account_ids": r.Remove, "scope_envelope": encoded(scope.Raw), "grant_envelopes": grants, "vault_envelope": vault, "confirm_total_loss": r.TotalLoss}, nil
	}
	return nil, env.ErrInvalid
}
