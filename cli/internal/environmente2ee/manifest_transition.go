package environmente2ee

import (
	"sort"
	"strings"
)

func (a Authority) ExpectedRecipients(scope ScopeKind, state ScopeState, machineID string) ([]Recipient, error) {
	result := []Recipient{}
	for _, b := range a.Bindings {
		r := Recipient{SubjectID: b.SubjectID, KeyGeneration: b.KeyGeneration, KeyID: b.RecipientKeyID, PublicKey: cloneBytes(b.RecipientPublic)}
		switch b.SubjectKind {
		case SubjectManagerCLI, SubjectManagerBrowser:
			r.Kind = RecipientManager
		case SubjectRecovery:
			r.Kind = RecipientRecovery
		case SubjectHost:
			if scope == ScopeGlobal {
				r.Kind = RecipientHost
			} else if state == ScopeActive && b.SubjectID == machineID {
				r.Kind = RecipientHost
			} else {
				continue
			}
		default:
			return nil, ErrInvalid
		}
		result = append(result, r)
	}
	sort.Slice(result, func(i, j int) bool { return recipientLess(result[i], result[j]) })
	if scope == ScopeMachine && state == ScopeActive {
		hosts := 0
		for _, r := range result {
			if r.Kind == RecipientHost {
				hosts++
			}
		}
		if hosts != 1 {
			return nil, ErrInvalid
		}
	}
	return result, nil
}

func ValidateManifestSuccessor(old *Manifest, next Manifest, authorityTransition bool) error {
	c := next.ManifestClaims
	if old == nil {
		return boolError(c.Mutation == MutationInitialize && c.PreviousVersion == 0 && c.Version == 1 && c.KeyEpoch == 1 && len(c.Names) == 0 && len(c.ChangedNames) == 0)
	}
	o := old.ManifestClaims
	if c.AccountID != o.AccountID || c.Scope != o.Scope || c.MachineID != o.MachineID || c.PreviousVersion != o.Version || c.Version != o.Version+1 {
		return ErrInvalid
	}
	if authorityTransition {
		if c.AuthorityGeneration != o.AuthorityGeneration+1 || c.AuthorityID == o.AuthorityID {
			return ErrInvalid
		}
	} else if c.AuthorityGeneration != o.AuthorityGeneration || c.AuthorityID != o.AuthorityID {
		return ErrInvalid
	}
	sameNames := equalStrings(c.Names, o.Names)
	switch c.Mutation {
	case MutationSet:
		if authorityTransition || c.KeyEpoch != o.KeyEpoch || c.State != o.State || len(c.ChangedNames) != 1 || !contains(c.Names, c.ChangedNames[0]) || !(sameNames || addedOne(o.Names, c.Names, c.ChangedNames[0])) {
			return ErrInvalid
		}
	case MutationUnset:
		if authorityTransition || c.KeyEpoch != o.KeyEpoch || c.State != o.State || len(c.ChangedNames) != 1 || contains(c.Names, c.ChangedNames[0]) || !removedOne(o.Names, c.Names, c.ChangedNames[0]) {
			return ErrInvalid
		}
	case MutationReauthorize:
		if !authorityTransition || !sameNames || len(c.ChangedNames) != 0 || c.KeyEpoch != o.KeyEpoch || !(c.State == o.State || o.State == ScopeRetired && c.State == ScopeActive) {
			return ErrInvalid
		}
	case MutationRotate:
		if !sameNames || len(c.ChangedNames) != 0 || c.KeyEpoch != o.KeyEpoch+1 || !(c.State == o.State || o.State == ScopeActive && c.State == ScopeRetired) {
			return ErrInvalid
		}
	case MutationReset:
		if !authorityTransition || len(c.Names) != 0 || !equalStrings(c.ChangedNames, o.Names) || c.KeyEpoch != o.KeyEpoch+1 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// ValidateManifestTransition validates a manifest together with the authority
// state that grants its signer and recipients. It is the validation entry point
// for clients and hosts applying a scope update. ValidateManifestSuccessor is
// retained as the narrower public-claim state-machine check.
func ValidateManifestTransition(old *Manifest, next Manifest, previousAuthority *Authority, proposed Authority) error {
	parsedNext, err := ParseManifest(next.Raw, proposed)
	if err != nil || parsedNext.ID != next.ID {
		return ErrInvalid
	}
	next = parsedNext

	authorityTransition := false
	if previousAuthority == nil {
		if ValidateAuthorityTransition(nil, proposed) != nil {
			return ErrInvalid
		}
	} else if previousAuthority.ID == proposed.ID {
		if previousAuthority.Generation != proposed.Generation {
			return ErrInvalid
		}
	} else {
		if ValidateAuthorityTransition(previousAuthority, proposed) != nil {
			return ErrInvalid
		}
		authorityTransition = true
	}

	resetAuthorized := authorityHasReset(proposed, next.Scope, next.MachineID)
	if old == nil {
		switch next.Mutation {
		case MutationInitialize:
			globalGenesis := previousAuthority == nil && proposed.Generation == 1 && next.Scope == ScopeGlobal
			machineEnrollment := authorityTransition && next.Scope == ScopeMachine
			if resetAuthorized || !globalGenesis && !machineEnrollment {
				return ErrInvalid
			}
			return ValidateManifestSuccessor(nil, next, authorityTransition)
		case MutationReset:
			if !authorityTransition || !resetAuthorized || next.PreviousVersion != 0 || next.Version != 1 || next.KeyEpoch != 1 || len(next.Names) != 0 || len(next.ChangedNames) != 0 {
				return ErrInvalid
			}
			return nil
		default:
			return ErrInvalid
		}
	}

	if previousAuthority == nil {
		return ErrInvalid
	}
	parsedOld, err := ParseManifest(old.Raw, *previousAuthority)
	if err != nil || parsedOld.ID != old.ID {
		return ErrInvalid
	}
	old = &parsedOld
	if old.AuthorityGeneration != previousAuthority.Generation || old.AuthorityID != previousAuthority.ID {
		return ErrInvalid
	}
	if resetAuthorized != (next.Mutation == MutationReset) {
		return ErrInvalid
	}
	if next.Mutation == MutationReset {
		if !authorityTransition {
			return ErrInvalid
		}
	} else if authorityTransition && removedRecipient(old.Wraps, next.Wraps) && next.Mutation != MutationRotate {
		return ErrInvalid
	}
	return ValidateManifestSuccessor(old, next, authorityTransition)
}

// MergeScopes returns a detached effective environment. Exact-name machine
// entries override global entries. ASCII case-fold collisions with different
// spellings are rejected, as are invalid or oversized merged environments.
func MergeScopes(global, machine map[string][]byte) (map[string][]byte, error) {
	if _, _, err := encodeScope(global); err != nil {
		return nil, ErrInvalid
	}
	if _, _, err := encodeScope(machine); err != nil {
		return nil, ErrInvalid
	}
	result := make(map[string][]byte, len(global)+len(machine))
	folded := make(map[string]string, len(global)+len(machine))
	for name, value := range global {
		result[name] = cloneBytes(value)
		folded[strings.ToUpper(name)] = name
	}
	for name, value := range machine {
		fold := strings.ToUpper(name)
		if existing, ok := folded[fold]; ok && existing != name {
			clearValues(result)
			return nil, ErrInvalid
		}
		result[name] = cloneBytes(value)
		folded[fold] = name
	}
	if _, raw, err := encodeScope(result); err != nil {
		clearValues(result)
		return nil, ErrInvalid
	} else {
		clear(raw)
	}
	return result, nil
}

// ValidateFirstHostDelivery validates the atomic global+machine bootstrap pair
// for a host whose exact recipient binding first appears in currentAuthority.
// The host has no prior manifest high-water, so this rule is rooted in the
// sequential account authority transition and both manager signatures.
func ValidateFirstHostDelivery(previousAuthority *Authority, currentAuthority Authority, global, machine Manifest, recipient RecipientPrivate) (FirstHostDelivery, error) {
	if ValidateAuthorityTransition(previousAuthority, currentAuthority) != nil || recipient.Kind != RecipientHost {
		return FirstHostDelivery{}, ErrInvalid
	}
	wasBound, isBound := false, false
	if previousAuthority != nil {
		for _, binding := range previousAuthority.Bindings {
			if binding.SubjectKind == SubjectHost && binding.SubjectID == recipient.SubjectID && binding.KeyGeneration == recipient.KeyGeneration && binding.RecipientKeyID == recipient.KeyID {
				wasBound = true
			}
		}
	}
	for _, binding := range currentAuthority.Bindings {
		if binding.SubjectKind == SubjectHost && binding.SubjectID == recipient.SubjectID && binding.KeyGeneration == recipient.KeyGeneration && binding.RecipientKeyID == recipient.KeyID {
			isBound = true
		}
	}
	if wasBound || !isBound {
		return FirstHostDelivery{}, ErrInvalid
	}
	parsedGlobal, err := ParseManifest(global.Raw, currentAuthority)
	if err != nil || parsedGlobal.ID != global.ID {
		return FirstHostDelivery{}, ErrInvalid
	}
	parsedMachine, err := ParseManifest(machine.Raw, currentAuthority)
	if err != nil || parsedMachine.ID != machine.ID {
		return FirstHostDelivery{}, ErrInvalid
	}
	global = parsedGlobal
	machine = parsedMachine
	// Reset is destructive and is authorized only by the root-signed scope
	// list on this authority transition. The generic manifest state-machine
	// check enforces this for managers, but hosts use the pair-delivery
	// validators below and must apply the same gate before decrypting.
	if global.Mutation == MutationReset && !authorityHasReset(currentAuthority, ScopeGlobal, "") ||
		machine.Mutation == MutationReset && !authorityHasReset(currentAuthority, ScopeMachine, recipient.SubjectID) {
		return FirstHostDelivery{}, ErrInvalid
	}
	validGlobal := global.Scope == ScopeGlobal && global.State == ScopeActive
	if previousAuthority == nil {
		validGlobal = validGlobal && global.Mutation == MutationInitialize && global.PreviousVersion == 0 && global.Version == 1 && global.KeyEpoch == 1 && len(global.Names) == 0 && len(global.ChangedNames) == 0
	} else {
		validGlobal = validGlobal && global.PreviousVersion > 0 && global.Version == global.PreviousVersion+1 && global.KeyEpoch > 0 &&
			(global.Mutation == MutationReauthorize || global.Mutation == MutationRotate || global.Mutation == MutationReset) &&
			(global.Mutation != MutationReauthorize && global.Mutation != MutationRotate || len(global.ChangedNames) == 0) &&
			(global.Mutation != MutationReset || len(global.Names) == 0)
	}
	if !validGlobal {
		return FirstHostDelivery{}, ErrInvalid
	}
	validMachine := machine.Scope == ScopeMachine && machine.MachineID == recipient.SubjectID && machine.State == ScopeActive
	if previousAuthority == nil {
		validMachine = validMachine && machine.Mutation == MutationInitialize && machine.PreviousVersion == 0 && machine.Version == 1 && machine.KeyEpoch == 1 && len(machine.Names) == 0 && len(machine.ChangedNames) == 0
	} else {
		validMachine = validMachine && validFirstPairTransitionShape(machine)
	}
	if !validMachine {
		return FirstHostDelivery{}, ErrInvalid
	}
	openedGlobal, err := DecryptManifest(global, recipient)
	if err != nil {
		return FirstHostDelivery{}, ErrInvalid
	}
	openedMachine, err := DecryptManifest(machine, recipient)
	if err != nil {
		clearValues(openedGlobal.Values)
		clear(openedGlobal.ScopeKey)
		return FirstHostDelivery{}, ErrInvalid
	}
	effective, err := MergeScopes(openedGlobal.Values, openedMachine.Values)
	if err != nil {
		clearValues(openedGlobal.Values)
		clear(openedGlobal.ScopeKey)
		clearValues(openedMachine.Values)
		clear(openedMachine.ScopeKey)
		return FirstHostDelivery{}, ErrInvalid
	}
	return FirstHostDelivery{Global: openedGlobal, Machine: openedMachine, Effective: effective}, nil
}

// ValidateLatestAfterFirstDelivery validates and decrypts the latest pair
// after a retained first-host bootstrap pair has established manifest version
// floors. The caller must already have verified the complete sequential
// authority chain between bootstrapAuthority and currentAuthority.
func ValidateLatestAfterFirstDelivery(bootstrapGlobal, bootstrapMachine, latestGlobal, latestMachine Manifest, bootstrapAuthority, currentAuthority Authority, recipient RecipientPrivate) (FirstHostDelivery, error) {
	if recipient.Kind != RecipientHost || bootstrapAuthority.AccountID != currentAuthority.AccountID || currentAuthority.Generation < bootstrapAuthority.Generation {
		return FirstHostDelivery{}, ErrInvalid
	}
	sameAuthority := currentAuthority.Generation == bootstrapAuthority.Generation
	if sameAuthority && currentAuthority.ID != bootstrapAuthority.ID {
		return FirstHostDelivery{}, ErrInvalid
	}
	bound := false
	for _, binding := range currentAuthority.Bindings {
		if binding.SubjectKind == SubjectHost && binding.SubjectID == recipient.SubjectID && binding.KeyGeneration == recipient.KeyGeneration && binding.RecipientKeyID == recipient.KeyID {
			bound = true
			break
		}
	}
	if !bound {
		return FirstHostDelivery{}, ErrInvalid
	}
	parsedGlobal, err := ParseManifest(latestGlobal.Raw, currentAuthority)
	if err != nil || parsedGlobal.ID != latestGlobal.ID {
		return FirstHostDelivery{}, ErrInvalid
	}
	parsedMachine, err := ParseManifest(latestMachine.Raw, currentAuthority)
	if err != nil || parsedMachine.ID != latestMachine.ID {
		return FirstHostDelivery{}, ErrInvalid
	}
	latestGlobal, latestMachine = parsedGlobal, parsedMachine
	// A reset must be authorized by the root-signed scope list. Genesis cannot
	// carry a reset manifest, but a reset authority can also be the bootstrap
	// authority for a newly appearing host binding, so do not require the
	// latest pair to be newer than bootstrapAuthority here.
	if latestGlobal.Mutation == MutationReset && (currentAuthority.Generation == 1 || !authorityHasReset(currentAuthority, ScopeGlobal, "") || sameAuthority && latestGlobal.ID != bootstrapGlobal.ID) ||
		latestMachine.Mutation == MutationReset && (currentAuthority.Generation == 1 || !authorityHasReset(currentAuthority, ScopeMachine, recipient.SubjectID) || sameAuthority && latestMachine.ID != bootstrapMachine.ID) {
		return FirstHostDelivery{}, ErrInvalid
	}
	// Genesis mutations can only be the exact bootstrap pair. A newer
	// authority already has the bootstrap scope as its predecessor, so an
	// initialize or PreviousVersion=0 reset there would bypass the manifest
	// transition state machine and could erase a host's established values.
	if !sameAuthority && (latestGlobal.Mutation == MutationInitialize || latestGlobal.Mutation == MutationReset && latestGlobal.PreviousVersion == 0 ||
		latestMachine.Mutation == MutationInitialize || latestMachine.Mutation == MutationReset && latestMachine.PreviousVersion == 0) {
		return FirstHostDelivery{}, ErrInvalid
	}
	if latestGlobal.Scope != ScopeGlobal || latestGlobal.State != ScopeActive || latestMachine.Scope != ScopeMachine || latestMachine.MachineID != recipient.SubjectID || latestMachine.State != ScopeActive ||
		latestGlobal.Version < bootstrapGlobal.Version || latestMachine.Version < bootstrapMachine.Version ||
		latestGlobal.Version == bootstrapGlobal.Version && latestGlobal.ID != bootstrapGlobal.ID ||
		latestMachine.Version == bootstrapMachine.Version && latestMachine.ID != bootstrapMachine.ID ||
		!validStandaloneManifestMutationShape(latestGlobal) || !validStandaloneManifestMutationShape(latestMachine) {
		return FirstHostDelivery{}, ErrInvalid
	}
	openedGlobal, err := DecryptManifest(latestGlobal, recipient)
	if err != nil {
		return FirstHostDelivery{}, ErrInvalid
	}
	openedMachine, err := DecryptManifest(latestMachine, recipient)
	if err != nil {
		clearValues(openedGlobal.Values)
		clear(openedGlobal.ScopeKey)
		return FirstHostDelivery{}, ErrInvalid
	}
	effective, err := MergeScopes(openedGlobal.Values, openedMachine.Values)
	if err != nil {
		clearValues(openedGlobal.Values)
		clear(openedGlobal.ScopeKey)
		clearValues(openedMachine.Values)
		clear(openedMachine.ScopeKey)
		return FirstHostDelivery{}, ErrInvalid
	}
	return FirstHostDelivery{Global: openedGlobal, Machine: openedMachine, Effective: effective}, nil
}

// ValidateLatestAfterHighWater applies the same monotonic latest-pair rule for
// an existing host whose durable old pair is already trusted. The caller must
// first verify and persist every sequential authority document through
// currentAuthority.
func ValidateLatestAfterHighWater(oldGlobal, oldMachine, latestGlobal, latestMachine Manifest, oldAuthority, currentAuthority Authority, recipient RecipientPrivate) (FirstHostDelivery, error) {
	return ValidateLatestAfterFirstDelivery(oldGlobal, oldMachine, latestGlobal, latestMachine, oldAuthority, currentAuthority, recipient)
}

func validFirstPairTransitionShape(manifest Manifest) bool {
	if manifest.Mutation == MutationInitialize {
		return manifest.PreviousVersion == 0 && manifest.Version == 1 && manifest.KeyEpoch == 1 && len(manifest.Names) == 0 && len(manifest.ChangedNames) == 0
	}
	if manifest.Mutation == MutationReset && manifest.PreviousVersion == 0 {
		// A root-authorized reset can initialize a scope that has no prior
		// manifest, such as a newly enrolled machine. ValidateFirstHostDelivery
		// applies the reset-scope authorization gate before reaching this shape
		// check, so this does not make an unauthorized genesis acceptable.
		return manifest.Version == 1 && manifest.KeyEpoch == 1 && len(manifest.Names) == 0 && len(manifest.ChangedNames) == 0
	}
	return manifest.PreviousVersion > 0 && manifest.Version == manifest.PreviousVersion+1 && manifest.KeyEpoch > 0 &&
		(manifest.Mutation == MutationReauthorize || manifest.Mutation == MutationRotate || manifest.Mutation == MutationReset) &&
		(manifest.Mutation != MutationReauthorize && manifest.Mutation != MutationRotate || len(manifest.ChangedNames) == 0) &&
		(manifest.Mutation != MutationReset || len(manifest.Names) == 0)
}

func validStandaloneManifestMutationShape(manifest Manifest) bool {
	if manifest.PreviousVersion+1 != manifest.Version || manifest.KeyEpoch == 0 {
		return false
	}
	switch manifest.Mutation {
	case MutationInitialize:
		return manifest.PreviousVersion == 0 && manifest.Version == 1 && manifest.KeyEpoch == 1 && len(manifest.Names) == 0 && len(manifest.ChangedNames) == 0
	case MutationSet:
		return len(manifest.ChangedNames) == 1 && contains(manifest.Names, manifest.ChangedNames[0])
	case MutationUnset:
		return len(manifest.ChangedNames) == 1 && !contains(manifest.Names, manifest.ChangedNames[0])
	case MutationReauthorize, MutationRotate:
		return len(manifest.ChangedNames) == 0
	case MutationReset:
		if manifest.PreviousVersion == 0 {
			return manifest.Version == 1 && manifest.KeyEpoch == 1 && len(manifest.Names) == 0 && len(manifest.ChangedNames) == 0
		}
		return len(manifest.Names) == 0
	default:
		return false
	}
}

func authorityHasReset(authority Authority, scope ScopeKind, machineID string) bool {
	for _, reset := range authority.ResetScopes {
		if reset.Scope == scope && reset.MachineID == machineID {
			return true
		}
	}
	return false
}

func removedRecipient(old, next []RecipientWrap) bool {
	for _, prior := range old {
		found := false
		for _, candidate := range next {
			if prior.Kind == candidate.Kind && prior.SubjectID == candidate.SubjectID && prior.KeyGeneration == candidate.KeyGeneration && prior.KeyID == candidate.KeyID {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}
