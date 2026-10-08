package main

import (
	"bytes"
	"context"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"strings"
)

type hostSelection struct {
	Kind  string `json:"owner_kind"`
	Owner string `json:"owner_id"`
	Name  string `json:"name"`
}
type hostState struct {
	Bundle    env.ProjectionBundle `json:"bundle"`
	Selection []hostSelection      `json:"selection"`
}

func (e *engine) provision(ctx context.Context, r request) (any, error) {
	if err := e.current(r.Vault); err != nil {
		return nil, err
	}
	if r.Host == nil || r.Operation == "" || len(r.Selection) > env.MaximumVariables || len(r.Inventory) > env.MaximumVaultTeams+2 || len(r.Teams) > env.MaximumVaultTeams {
		return nil, env.ErrInvalid
	}
	b := r.Host.Bundle
	if b.AccountID != e.head.AccountID || b.MachineID != r.Machine || b.State == "revoked" {
		return nil, env.ErrInvalid
	}
	public, err := decode(b.HostPublic)
	if err != nil {
		return nil, err
	}
	var previous env.DocumentID
	if b.ProjectionRevision > 0 {
		previous, err = env.ParseDocumentID(b.DocumentID)
		if err != nil {
			return nil, err
		}
	}
	scopes := map[string]map[string][]byte{}
	documents := map[string]env.VaultScope{}
	defer func() {
		for _, values := range scopes {
			for _, value := range values {
				clear(value)
			}
		}
	}()
	for _, state := range r.Inventory {
		coordinate := state.Kind + "\x00" + state.Owner + "\x00" + state.Machine
		if _, exists := scopes[coordinate]; exists {
			return nil, env.ErrInvalid
		}
		doc, err := scopeDocument(state)
		if err != nil || doc.Claims.Issuer != e.head.Issuer {
			return nil, env.ErrInvalid
		}
		var key []byte
		epoch := e.keys.PersonalEpoch
		if state.Kind == "personal" {
			if state.Owner != e.head.AccountID || state.Machine != "" && state.Machine != r.Machine {
				return nil, env.ErrInvalid
			}
			key = e.keys.PersonalKey
		} else if state.Kind == "team" {
			if state.Machine != "" {
				return nil, env.ErrInvalid
			}
			for _, team := range r.Teams {
				if team.ID == state.Owner {
					key, _, err = e.teamKey(team, "read")
					epoch = team.Epoch
					if err != nil {
						return nil, err
					}
				}
			}
			if key == nil {
				return nil, env.ErrInvalid
			}
		} else {
			return nil, env.ErrInvalid
		}
		if state.Epoch != epoch {
			return nil, env.ErrInvalid
		}
		values, err := env.OpenVaultScope(ctx, doc, key)
		if err != nil {
			return nil, err
		}
		scopes[coordinate] = values
		documents[coordinate] = doc
	}
	values := map[string][]byte{}
	defer func() {
		for _, v := range values {
			clear(v)
		}
	}()
	seen := map[string]bool{}
	sources := []env.ProjectionSource{}
	used := map[string]bool{}
	addSource := func(coordinate string) {
		if used[coordinate] {
			return
		}
		used[coordinate] = true
		doc := documents[coordinate]
		c := doc.Claims
		sources = append(sources, env.ProjectionSource{OwnerKind: c.OwnerKind, OwnerID: c.OwnerID, MachineID: c.MachineID, KeyEpoch: c.KeyEpoch, Revision: c.Revision, Digest: doc.ID[:]})
	}
	for _, selected := range r.Selection {
		if seen[strings.ToUpper(selected.Name)] {
			return nil, env.ErrInvalid
		}
		seen[strings.ToUpper(selected.Name)] = true
		coordinate := selected.Kind + "\x00" + selected.Owner + "\x00"
		value, ok := scopes[coordinate][selected.Name]
		if !ok {
			return nil, env.ErrInvalid
		}
		values[selected.Name] = bytes.Clone(value)
		addSource(coordinate)
	}
	machineCoordinate := "personal\x00" + e.head.AccountID + "\x00" + r.Machine
	overrides := scopes[machineCoordinate]
	if overrides != nil {
		addSource(machineCoordinate)
	} else {
		overrides = map[string][]byte{}
	}
	effective, err := env.MergeScopes(values, overrides)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, v := range effective {
			clear(v)
		}
	}()
	projection, err := env.SealHostProjection(ctx, env.HostProjectionClaims{Issuer: e.head.Issuer, OwnerAccount: e.head.AccountID, MachineID: r.Machine, InstallationGeneration: b.InstallationGeneration, HostKeyGeneration: b.HostKeyGeneration, HostPublic: public, SelectionGeneration: b.SelectionGeneration + 1, Revision: b.ProjectionRevision + 1, Previous: previous[:], Sources: sources, WriterVaultGeneration: e.head.Generation}, e.keys.WriterSeed, effective)
	if err != nil {
		return nil, err
	}
	return map[string]any{"operation_id": r.Operation, "expected_selection_generation": b.SelectionGeneration, "selection": r.Selection, "envelope": encoded(projection.Raw)}, nil
}
