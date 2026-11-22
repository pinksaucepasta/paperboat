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

// machineIDPrefix starts every stable machine ID the server issues ("machine_" + a lowercase UUID).
const machineIDPrefix = "machine_"

// validMachineID accepts the stable machine ID form: "machine_" followed by lowercase letters, digits and
// hyphens, not starting with a hyphen. It is a shape check only; the server decides whether the machine exists.
func validMachineID(value string) bool {
	rest, found := strings.CutPrefix(value, machineIDPrefix)
	if !found || rest == "" || len(rest) > 64 || rest[0] == '-' {
		return false
	}
	for _, character := range rest {
		if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

// ValidMachineReference reports whether value can name a Paperboat machine on a managed SSH command line
// (pb ssh, scp, sftp, rsync): a machine name/alias (a DNS label) or a stable machine ID. Nothing else is
// resolved, so an operand starting with '-' or carrying shell or OpenSSH syntax never reaches OpenSSH.
// Alias creation keeps the stricter ValidateAlias.
func ValidMachineReference(value string) bool {
	return validAliasLabel(value) || validMachineID(value)
}

// ParseMachineReference parses a [user@]machine operand shared by every managed SSH command. A trailing
// ".<domain>" (the managed SSH namespace) is removed when domain is set. The returned target is lowercase:
// aliases match case-insensitively and machine IDs are lowercase.
func ParseMachineReference(value, domain string) (target, username string, err error) {
	target, username, err = ParseMachineTarget(value)
	if err != nil {
		return "", "", err
	}
	target = strings.ToLower(target)
	if domain != "" {
		target = strings.TrimSuffix(target, "."+strings.ToLower(domain))
	}
	if !ValidMachineReference(target) {
		return "", "", ErrMachineReferenceInvalid
	}
	return target, username, nil
}
