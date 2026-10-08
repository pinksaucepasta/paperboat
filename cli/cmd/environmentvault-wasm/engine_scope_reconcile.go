package main

import (
	"crypto/ed25519"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

func (e *engine) reconcileScope(r request) (any, error) {
	if r.Action == "scope-candidate" {
		if err := e.current(r.Vault); err != nil {
			return nil, err
		}
	} else {
		head, _, err := r.Vault.verified()
		if err != nil || len(e.raw) == 0 || head.AccountID != e.head.AccountID || head.Issuer != e.head.Issuer {
			return nil, env.ErrInvalid
		}
	}
	if r.Kind != "personal" && r.Kind != "team" || r.Kind == "personal" && r.Owner != e.head.AccountID {
		return nil, env.ErrInvalid
	}
	if r.Action == "scope-candidate" {
		raw, err := decode(r.Envelope)
		if err != nil {
			return nil, err
		}
		signer := ed25519.NewKeyFromSeed(e.keys.WriterSeed)
		defer clear(signer)
		writer := signer.Public().(ed25519.PublicKey)
		document, err := env.ParseVaultScope(raw, writer)
		if err != nil {
			return nil, err
		}
		c := document.Claims
		if c.Issuer != e.head.Issuer || c.OwnerKind != r.Kind || c.OwnerID != r.Owner || c.MachineID != r.Machine || c.WriterAccount != e.head.AccountID || c.WriterVaultGeneration != e.head.Generation {
			return nil, env.ErrInvalid
		}
		return scopeState{WriterPublic: encoded(writer), Kind: c.OwnerKind, Owner: c.OwnerID, Machine: c.MachineID, Epoch: c.KeyEpoch, Revision: c.Revision, DocumentID: digest(document.ID), Envelope: r.Envelope}, nil
	}
	if r.Scope == nil || r.Candidate == nil {
		return nil, env.ErrInvalid
	}
	candidate, err := scopeDocument(*r.Candidate)
	if err != nil {
		return nil, err
	}
	current, err := scopeDocument(*r.Scope)
	if err != nil {
		return nil, err
	}
	for _, c := range []env.VaultScopeClaims{candidate.Claims, current.Claims} {
		if c.Issuer != e.head.Issuer || c.OwnerKind != r.Kind || c.OwnerID != r.Owner || c.MachineID != r.Machine {
			return nil, env.ErrInvalid
		}
	}
	if candidate.Claims.WriterAccount != e.head.AccountID {
		return nil, env.ErrInvalid
	}
	state := "retained"
	if current.ID == candidate.ID {
		state = "committed"
	} else if current.Claims.KeyEpoch > candidate.Claims.KeyEpoch || current.Claims.KeyEpoch == candidate.Claims.KeyEpoch && current.Claims.Revision >= candidate.Claims.Revision {
		state = "superseded"
	}
	return map[string]string{"state": state}, nil
}
