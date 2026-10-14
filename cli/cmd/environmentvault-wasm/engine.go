package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"

	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type vaultState struct {
	Issuer     string `json:"issuer"`
	Account    string `json:"account_id"`
	Generation uint64 `json:"generation"`
	DocumentID string `json:"document_id"`
	Envelope   string `json:"envelope"`
}
type scopeState struct {
	WorkspaceID  string `json:"workspace_id"`
	WriterPublic string `json:"writer_public"`
	Kind         string `json:"owner_kind"`
	Owner        string `json:"owner_id"`
	Machine      string `json:"machine_id"`
	Epoch        uint64 `json:"key_epoch"`
	Revision     uint64 `json:"revision"`
	DocumentID   string `json:"document_id"`
	Envelope     string `json:"envelope"`
}
type request struct {
	Records        []recordState   `json:"records"`
	RecordSequence uint64          `json:"record_sequence"`
	Record         *recordState    `json:"record"`
	Deleted        bool            `json:"deleted"`
	WorkspaceID    string          `json:"workspace_id"`
	LayerRecipient *layerRecipient `json:"recipient_metadata"`
	Envelope       string          `json:"envelope"`
	Candidate      *scopeState     `json:"candidate"`
	Teams          []teamState     `json:"teams"`

	Team       *teamState     `json:"team"`
	Sharing    []sharingState `json:"sharing"`
	Grant      *grantState    `json:"grant"`
	Recipient  string         `json:"recipient"`
	Operation  string         `json:"operation_id"`
	Membership uint64         `json:"membership_generation"`
	Remove     []string       `json:"remove_account_ids"`
	TotalLoss  bool           `json:"confirm_total_loss"`

	Action         string       `json:"action"`
	Vault          vaultState   `json:"vault"`
	Scope          *scopeState  `json:"scope"`
	Kind           string       `json:"kind"`
	Owner          string       `json:"owner"`
	Machine        string       `json:"machine"`
	Name           string       `json:"name"`
	Value          string       `json:"value"`
	Password       string       `json:"password"`
	Code           string       `json:"code"`
	Inventory      []scopeState `json:"inventory"`
	KeyEpoch       uint64       `json:"key_epoch"`
	EnableRecovery bool         `json:"enable_recovery"`
}
type engine struct {
	mu          sync.Mutex
	keys        env.VaultKeys
	head        env.VaultHead
	raw         []byte
	pendingRaw  []byte
	pendingHead env.VaultHead
	pendingKeys *env.VaultKeys
}

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.Strict().DecodeString(s) }
func encoded(raw []byte) string       { return base64.RawURLEncoding.EncodeToString(raw) }
func digest(id env.DocumentID) string { return "sha256:" + hex.EncodeToString(id[:]) }
func (s vaultState) verified() (env.VaultHead, []byte, error) {
	raw, err := decode(s.Envelope)
	if err != nil {
		return env.VaultHead{}, nil, env.ErrInvalid
	}
	head, _, err := env.InspectPasswordVault(raw)
	if err != nil || head.Issuer != s.Issuer || head.AccountID != s.Account || head.Generation != s.Generation || digest(head.ID) != s.DocumentID {
		return env.VaultHead{}, nil, env.ErrInvalid
	}
	return head, raw, nil
}
func (e *engine) clear() {
	e.keys.Clear()
	clear(e.raw)
	clear(e.pendingRaw)
	if e.pendingKeys != nil {
		e.pendingKeys.Clear()
	}
	e.raw = nil
	e.pendingRaw = nil
	e.pendingKeys = nil
	e.head = env.VaultHead{}
}
func (e *engine) current(s vaultState) error {
	if s.Account != e.head.AccountID {
		e.clear()
		return errors.New("ENV account changed. Browser keys were locked.")
	}
	h, _, err := s.verified()
	if err != nil || h != e.head || len(e.raw) == 0 {
		return errors.New("ENV vault changed or is locked. Unlock its current version before retrying.")
	}
	if e.pendingRaw != nil {
		return errors.New("An encrypted vault update is pending. Retry that publication or lock and unlock the current vault.")
	}
	return nil
}
func (e *engine) stage(ctx context.Context, h env.VaultHead, previous env.DocumentID, protection env.VaultProtection, keys env.VaultKeys) (any, error) {
	payload, err := keys.MarshalBinary()
	if err != nil {
		return nil, err
	}
	defer clear(payload)
	raw, next, err := env.SealProtectedVault(ctx, h, previous, protection, payload)
	if err != nil {
		return nil, err
	}
	e.pendingRaw = raw
	e.pendingHead = next
	return map[string]any{"envelope": encoded(raw), "document_id": digest(next.ID)}, nil
}
func (e *engine) run(ctx context.Context, r request) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	switch r.Action {
	case "record-id", "record-read", "record-write", "scope-create":
		return e.recordOperation(ctx, r)
	case "scope-candidate", "scope-reconcile":
		return e.reconcileScope(r)
	case "personal-rotate", "personal-rotate-scope":
		return e.rotatePersonal(ctx, r)
	case "status":
		account := e.head.AccountID
		if e.pendingRaw != nil {
			account = e.pendingHead.AccountID
		}
		if account != r.Vault.Account {
			e.clear()
		}
		recovery := false
		if len(e.raw) > 0 {
			p, err := env.PasswordVaultProtection(e.raw)
			recovery = err == nil && p.Recovery != nil
		}
		return map[string]any{"unlocked": len(e.raw) > 0 && e.head.AccountID == r.Vault.Account, "recovery_enabled": recovery}, nil
	case "lock":
		e.clear()
		return map[string]bool{"locked": true}, nil
	case "recovery-code":
		code, err := env.GenerateVaultRecoveryCode()
		defer clear(code)
		if err != nil {
			return nil, err
		}
		return map[string]string{"code": string(code)}, nil
	case "commit":
		h, raw, err := r.Vault.verified()
		if err == nil && e.pendingRaw == nil && h == e.head && len(e.raw) > 0 && bytes.Equal(raw, e.raw) {
			return map[string]bool{"unlocked": true}, nil
		}
		if err != nil || e.pendingRaw == nil || h != e.pendingHead || !bytes.Equal(raw, e.pendingRaw) {
			return nil, env.ErrInvalid
		}
		if e.pendingKeys != nil {
			e.keys.Clear()
			e.keys = *e.pendingKeys
			e.pendingKeys = nil
		}
		clear(e.raw)
		e.raw = raw
		e.head = h
		clear(e.pendingRaw)
		e.pendingRaw = nil
		return map[string]bool{"unlocked": true}, nil
	case "initialize":
		if e.pendingRaw != nil {
			return nil, errors.New("A vault initialization is already pending.")
		}
		if r.Vault.Issuer == "" || r.Vault.Account == "" || len(r.Password) < 1 {
			return nil, env.ErrInvalid
		}
		keys, err := env.NewVaultKeys()
		if err != nil {
			return nil, err
		}
		password := []byte(r.Password)
		defer clear(password)
		code := []byte(r.Code)
		defer clear(code)
		h := env.VaultHead{Issuer: r.Vault.Issuer, AccountID: r.Vault.Account, Generation: 1}
		protection, err := env.NewVaultProtection(ctx, h, password, code, keys.WriterSeed)
		if err != nil {
			keys.Clear()
			return nil, err
		}
		out, err := e.stage(ctx, h, env.DocumentID{}, protection, keys)
		if err != nil {
			keys.Clear()
			return nil, err
		}
		e.pendingKeys = &keys
		return out, nil
	case "reset-vault":
		if e.pendingRaw != nil {
			return nil, errors.New("A vault update is already pending.")
		}
		old, raw, err := r.Vault.verified()
		if err != nil || r.KeyEpoch == 0 || len(r.Inventory) > 513 {
			return nil, env.ErrInvalid
		}
		oldProtection, err := env.PasswordVaultProtection(raw)
		if err != nil {
			return nil, err
		}
		keys, err := env.NewVaultKeys()
		if err != nil {
			return nil, err
		}
		keys.PersonalEpoch = r.KeyEpoch + 1
		signer := ed25519.NewKeyFromSeed(keys.WriterSeed)
		defer clear(signer)
		protection := env.VaultProtection{WriterPublic: signer.Public().(ed25519.PublicKey), Password: env.VaultCredential{Epoch: oldProtection.Password.Epoch}, RecoveryEpoch: oldProtection.RecoveryEpoch + 1}
		password := []byte(r.Password)
		defer clear(password)
		protection, err = env.ReplaceVaultPassword(ctx, old, protection, password)
		if err != nil {
			keys.Clear()
			return nil, err
		}
		h := old
		h.Generation++
		h.ID = env.DocumentID{}
		out, err := e.stage(ctx, h, old.ID, protection, keys)
		if err != nil {
			keys.Clear()
			return nil, err
		}
		e.pendingKeys = &keys
		replacements := []string{}
		seen := map[string]bool{}
		for _, state := range r.Inventory {
			if state.Kind != "personal" || state.Owner != old.AccountID || state.Epoch != r.KeyEpoch || state.Revision == 0 || seen[state.WorkspaceID+"\x00"+state.Machine] {
				e.clear()
				return nil, env.ErrInvalid
			}
			seen[state.WorkspaceID+"\x00"+state.Machine] = true
			previous, err := env.ParseDocumentID(state.DocumentID)
			if err != nil {
				e.clear()
				return nil, err
			}
			scope, err := env.SealVaultScope(ctx, env.VaultScopeClaims{Issuer: old.Issuer, OwnerKind: "personal", OwnerID: old.AccountID, MachineID: state.Machine, WorkspaceID: state.WorkspaceID, KeyEpoch: keys.PersonalEpoch, Revision: state.Revision + 1, Previous: previous[:], WriterAccount: old.AccountID, WriterVaultGeneration: h.Generation}, keys.PersonalKey, keys.WriterSeed, map[string][]byte{})
			if err != nil {
				e.clear()
				return nil, err
			}
			replacements = append(replacements, encoded(scope.Raw))
		}
		result := out.(map[string]any)
		result["scope_envelopes"] = replacements
		return result, nil
	case "unlock", "recover":
		h, raw, err := r.Vault.verified()
		if err != nil {
			return nil, err
		}
		secret := []byte(r.Password)
		if r.Action == "recover" {
			secret = []byte(r.Code)
		}
		defer clear(secret)
		var payload []byte
		if r.Action == "recover" {
			payload, err = env.OpenRecoveryVault(ctx, h, secret, raw)
		} else {
			payload, err = env.OpenPasswordVault(ctx, h, secret, raw)
		}
		defer clear(payload)
		if err != nil {
			return nil, errors.New("ENV unlock failed. Check the password or recovery code.")
		}
		keys, err := env.ParseVaultKeys(payload)
		if err != nil {
			return nil, err
		}
		e.clear()
		e.keys = keys
		e.head = h
		e.raw = raw
		protection, err := env.PasswordVaultProtection(raw)
		if err != nil {
			return nil, err
		}
		return map[string]any{"unlocked": true, "recovery_enabled": protection.Recovery != nil}, nil
	case "password", "recovery":
		if err := e.current(r.Vault); err != nil {
			return nil, err
		}
		protection, err := env.PasswordVaultProtection(e.raw)
		if err != nil {
			return nil, err
		}
		if r.Action == "password" {
			secret := []byte(r.Password)
			defer clear(secret)
			protection, err = env.ReplaceVaultPassword(ctx, e.head, protection, secret)
		} else {
			code := []byte(r.Code)
			defer clear(code)
			protection, err = env.ReplaceVaultRecovery(ctx, e.head, protection, code)
		}
		if err != nil {
			return nil, err
		}
		h := e.head
		h.Generation++
		h.ID = env.DocumentID{}
		return e.stage(ctx, h, e.head.ID, protection, e.keys)
	case "host-layer":
		return e.hostLayer(ctx, r)
	case "team-create", "team-grant", "team-accept", "team-rotate":
		return e.teamOperation(ctx, r)

	}
	return nil, env.ErrInvalid
}
