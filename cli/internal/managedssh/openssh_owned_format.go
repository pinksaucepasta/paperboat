package managedssh

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The Paperboat-owned OpenSSH fragment (~/.ssh/paperboat_config) has one format on every platform:
//
//	# BEGIN paperboat-managed-ssh-v1
//	Host <alias>.<domain>                 one group per machine
//	    Port <n>
//	Match host <alias>.<domain> user "<setup user>"
//	    IdentityAgent "<pb agent>"        the managed key is offered only to the user who set pb up
//	    IdentityFile "<public key file>"
//	    IdentitiesOnly yes
//	    BatchMode yes
//	    PasswordAuthentication no
//	    KbdInteractiveAuthentication no
//	Host *.<domain>                       transport and host-key trust for every user
//	    ProxyCommand ...
//	    KnownHostsCommand ...
//	    StrictHostKeyChecking yes
//	    ...
//	# END paperboat-managed-ssh-v1
//
// Any other OS user reaches the machine over the same ProxyCommand and authenticates with their own
// key, agent or password: nothing here forces or offers the managed key to them.
type ownedOpenSSH struct {
	Targets           []OpenSSHAliasTarget
	ProxyCommand      string
	KnownHostsCommand string
	Agent             string
	Identity          string
}

const ownedOpenSSHTransport = "    StrictHostKeyChecking yes\n    CheckHostIP no\n    UserKnownHostsFile none\n    GlobalKnownHostsFile none\n    CanonicalizeHostname yes\n"

const ownedOpenSSHManagedAuth = "    IdentitiesOnly yes\n    BatchMode yes\n    PasswordAuthentication no\n    KbdInteractiveAuthentication no\n"

func quoteOpenSSHPath(value string) string {
	return "\"" + strings.ReplaceAll(value, "\\", "\\\\") + "\""
}

func unquoteOpenSSHPath(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix+"\"") || !strings.HasSuffix(line, "\"") {
		return "", false
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(line, prefix+"\""), "\"")
	if encoded == "" || strings.ContainsRune(encoded, '"') {
		return "", false
	}
	decoded := strings.ReplaceAll(encoded, `\\`, `\`)
	return decoded, strings.ReplaceAll(decoded, `\`, `\\`) == encoded
}

// ManagedIdentityArguments are the fragment's managed-key options as OpenSSH command-line options. A connection
// that overrides Hostname needs them there: `Match host` compares the overridden name (ssh_config(5)), so the
// fragment's Match group, which offers the managed key, never applies to it.
func ManagedIdentityArguments(agent, identity string) []string {
	return []string{"-o", "IdentityAgent=" + quoteOpenSSHPath(agent), "-o", "IdentityFile=" + quoteOpenSSHPath(identity)}
}

// matchUserPattern is the `Match user` pattern list for a registered account: the account as registered and, for an
// AnyCaseUser target, its lowercase, uppercase and capitalized spellings (the ones people type). shortcut: a mixed
// spelling such as "PuJan" is not covered; OpenSSH has no case-insensitive pattern, upgrade only if one appears.
func matchUserPattern(target OpenSSHAliasTarget) string {
	patterns := []string{target.User}
	if target.AnyCaseUser {
		lower := strings.ToLower(target.User)
		for _, spelling := range []string{lower, strings.ToUpper(target.User), strings.ToUpper(lower[:1]) + lower[1:]} {
			if !slices.Contains(patterns, spelling) {
				patterns = append(patterns, spelling)
			}
		}
	}
	return strings.Join(patterns, ",")
}

// formatOwnedOpenSSH renders the fragment. Callers validate the inputs first.
func formatOwnedOpenSSH(domain string, owned ownedOpenSSH) ([]byte, error) {
	var out strings.Builder
	out.WriteString(openSSHBeginMarker + "\n")
	seen := make(map[string]struct{}, len(owned.Targets))
	for _, target := range owned.Targets {
		host, err := AliasHost(target.Alias, domain)
		if err != nil || target.Port == 0 || !ValidUsername(target.User) {
			return nil, ErrOpenSSHConfigConflict
		}
		if _, exists := seen[host]; exists {
			return nil, ErrOpenSSHConfigConflict
		}
		seen[host] = struct{}{}
		fmt.Fprintf(&out, "Host %s\n    Port %d\nMatch host %s user \"%s\"\n", host, target.Port, host, matchUserPattern(target))
		out.WriteString("    IdentityAgent " + quoteOpenSSHPath(owned.Agent) + "\n")
		out.WriteString("    IdentityFile " + quoteOpenSSHPath(owned.Identity) + "\n")
		out.WriteString(ownedOpenSSHManagedAuth)
	}
	out.WriteString("Host *." + domain + "\n")
	out.WriteString("    ProxyCommand " + owned.ProxyCommand + "\n")
	out.WriteString("    KnownHostsCommand " + owned.KnownHostsCommand + "\n")
	out.WriteString(ownedOpenSSHTransport)
	out.WriteString(openSSHEndMarker + "\n")
	return []byte(out.String()), nil
}

// parseOwnedOpenSSH reads a fragment in the current format and accepts it only when rendering what
// was read gives back exactly the same bytes, so no line outside the format can pass.
func parseOwnedOpenSSH(value []byte, domain string) (ownedOpenSSH, bool) {
	lines := strings.Split(strings.TrimSuffix(string(value), "\n"), "\n")
	var owned ownedOpenSSH
	index := 1
	for index+8 < len(lines) && strings.HasPrefix(lines[index], "Host ") && lines[index] != "Host *."+domain {
		host := strings.TrimPrefix(lines[index], "Host ")
		alias, err := ParseAliasHost(host, domain)
		port, portErr := strconv.ParseUint(strings.TrimPrefix(lines[index+1], "    Port "), 10, 16)
		prefix := "Match host " + host + " user \""
		if err != nil || portErr != nil || !strings.HasPrefix(lines[index+1], "    Port ") || !strings.HasPrefix(lines[index+2], prefix) || !strings.HasSuffix(lines[index+2], "\"") {
			return ownedOpenSSH{}, false
		}
		user, _, anyCase := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(lines[index+2], prefix), "\""), ",")
		agent, agentOK := unquoteOpenSSHPath(lines[index+3], "    IdentityAgent ")
		identity, identityOK := unquoteOpenSSHPath(lines[index+4], "    IdentityFile ")
		if !agentOK || !identityOK {
			return ownedOpenSSH{}, false
		}
		if len(owned.Targets) == 0 {
			owned.Agent, owned.Identity = agent, identity
		}
		owned.Targets = append(owned.Targets, OpenSSHAliasTarget{Alias: alias, User: user, Port: uint16(port), AnyCaseUser: anyCase})
		index += 9
	}
	if index+3 >= len(lines) || lines[index] != "Host *."+domain || !strings.HasPrefix(lines[index+1], "    ProxyCommand ") || !strings.HasPrefix(lines[index+2], "    KnownHostsCommand ") {
		return ownedOpenSSH{}, false
	}
	owned.ProxyCommand = strings.TrimPrefix(lines[index+1], "    ProxyCommand ")
	owned.KnownHostsCommand = strings.TrimPrefix(lines[index+2], "    KnownHostsCommand ")
	rendered, err := formatOwnedOpenSSH(domain, owned)
	if err != nil || string(rendered) != string(value) {
		return ownedOpenSSH{}, false
	}
	return owned, true
}

// ownedOpenSSHIdentityMatches reports whether the installed fragment offers exactly this agent and
// public key file (every machine group names the same pair).
func ownedOpenSSHIdentityMatches(owned ownedOpenSSH, agent, identity string) bool {
	return len(owned.Targets) == 0 || owned.Agent == agent && owned.Identity == identity
}
