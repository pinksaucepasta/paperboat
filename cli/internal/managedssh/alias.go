package managedssh

import "strings"

func validAliasSuffix(value string) bool {
	if value == "" || len(value) > 253 || value != strings.ToLower(value) || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !validAliasLabel(label) {
			return false
		}
	}
	return true
}

// ValidateAliasDomain accepts only a canonical, explicitly discovered DNS namespace.
func ValidateAliasDomain(domain string) error {
	if !validAliasSuffix(domain) || !strings.Contains(domain, ".") {
		return ErrSSHAliasInvalid
	}
	return nil
}

func ValidateAlias(alias string) error {
	if !validAliasLabel(strings.ToLower(strings.TrimSpace(alias))) {
		return ErrSSHAliasInvalid
	}
	return nil
}
