package environmente2ee

import (
	"sort"
)

func validateRecipient(r Recipient) error {
	kid, e := KeyIDX25519(r.PublicKey)
	if e != nil || kid != r.KeyID || !validIdentifier(r.SubjectID) || r.KeyGeneration == 0 || r.KeyGeneration > MaximumContractInteger || r.Kind < RecipientManager || r.Kind > RecipientRecovery {
		return ErrInvalid
	}
	return nil
}

func validateWrap(w RecipientWrap) error {
	if !validIdentifier(w.SubjectID) || !validKeyID(w.KeyID) || w.KeyGeneration == 0 || w.KeyGeneration > MaximumContractInteger || w.Kind < RecipientManager || w.Kind > RecipientRecovery || len(w.EncapsulatedKey) != 32 || len(w.Ciphertext) != 48 {
		return ErrInvalid
	}
	return nil
}

func recipientLess(a, b Recipient) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.SubjectID != b.SubjectID {
		return a.SubjectID < b.SubjectID
	}
	if a.KeyGeneration != b.KeyGeneration {
		return a.KeyGeneration < b.KeyGeneration
	}
	return a.KeyID < b.KeyID
}

func wrapLess(a, b RecipientWrap) bool {
	return recipientLess(Recipient{Kind: a.Kind, SubjectID: a.SubjectID, KeyGeneration: a.KeyGeneration, KeyID: a.KeyID}, Recipient{Kind: b.Kind, SubjectID: b.SubjectID, KeyGeneration: b.KeyGeneration, KeyID: b.KeyID})
}

func equalWrapRecipients(w []RecipientWrap, r []Recipient) bool {
	if len(w) != len(r) {
		return false
	}
	for i := range w {
		if w[i].Kind != r[i].Kind || w[i].SubjectID != r[i].SubjectID || w[i].KeyGeneration != r[i].KeyGeneration || w[i].KeyID != r[i].KeyID {
			return false
		}
	}
	return true
}

func machineValue(c ManifestClaims) any {
	if c.Scope == ScopeMachine {
		return c.MachineID
	}
	return nil
}

func stringsAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func parseStringArray(v any) ([]string, error) {
	items, e := arrayAny(v)
	if e != nil {
		return nil, e
	}
	out := make([]string, len(items))
	for i, x := range items {
		out[i], e = text(x)
		if e != nil {
			return nil, e
		}
	}
	return out, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(a []string, x string) bool {
	i := sort.SearchStrings(a, x)
	return i < len(a) && a[i] == x
}

func addedOne(old, next []string, name string) bool {
	copyOld := append([]string(nil), old...)
	copyOld = append(copyOld, name)
	sort.Strings(copyOld)
	return equalStrings(copyOld, next)
}

func removedOne(old, next []string, name string) bool {
	out := make([]string, 0, len(old)-1)
	removed := false
	for _, x := range old {
		if x == name && !removed {
			removed = true
			continue
		}
		out = append(out, x)
	}
	return removed && equalStrings(out, next)
}

func containsInt(a []int, x int) bool {
	for _, v := range a {
		if v == x {
			return true
		}
	}
	return false
}

func boolError(ok bool) error {
	if !ok {
		return ErrInvalid
	}
	return nil
}

func clearValues(values map[string][]byte) {
	for _, value := range values {
		clear(value)
	}
}
