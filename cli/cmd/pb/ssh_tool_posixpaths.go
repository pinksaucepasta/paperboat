package main

import "strings"

// rsync on Windows is usually MSYS2 or Cygwin's: it reads "C:\dir" as host "C", path "\dir".
// Local drive-letter operands are converted with the cygpath that ships beside it.
func convertDriveOperands(args []string, convert func(string) (string, error)) ([]string, error) {
	result := append([]string(nil), args...)
	for index, value := range result {
		prefix := ""
		if strings.HasPrefix(value, "--") { // --exclude-from=C:\x, --files-from=C:\x, --backup-dir=C:\x
			if eq := strings.IndexByte(value, '='); eq > 0 {
				prefix, value = value[:eq+1], value[eq+1:]
			}
		}
		if !isDriveOperand(value) {
			continue
		}
		converted, err := convert(value)
		if err != nil {
			return nil, err
		}
		result[index] = prefix + converted
	}
	return result, nil
}

func isDriveOperand(value string) bool {
	if len(value) < 3 || value[1] != ':' || value[2] != '\\' && value[2] != '/' {
		return false
	}
	letter := value[0] | 0x20
	return letter >= 'a' && letter <= 'z'
}

// rsyncShortValueOptions are rsync's short options that take a value (rsync options.c): in a cluster the first of
// them takes the rest of the cluster, or the next argument when it is last.
const rsyncShortValueOptions = "eBfTM@"

// rsyncRemoteShellChosen reports whether the user picked rsync's remote shell (-e, --rsh, a non-empty RSYNC_RSH);
// pb then leaves it alone.
func rsyncRemoteShellChosen(args, environment []string) bool {
	for _, entry := range environment {
		if key, value, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, "RSYNC_RSH") && value != "" {
			return true
		}
	}
	for index := 0; index < len(args); index++ {
		value := args[index]
		if value == "--" {
			return false
		}
		if value == "--rsh" || strings.HasPrefix(value, "--rsh=") {
			return true
		}
		if !strings.HasPrefix(value, "-") || value == "-" {
			continue
		}
		if !strings.HasPrefix(value, "--") {
			// "-ave ssh" picks the shell; in "-Te" or "-Me" the e is the value of -T or -M.
			if at := strings.IndexAny(value[1:], rsyncShortValueOptions); at >= 0 && value[1+at] == 'e' {
				return true
			}
		}
		if takesNext, _ := rawToolOption("rsync", value); takesNext {
			index++ // the next argument is this option's value, even when it starts with -e
		}
	}
	return false
}

// rsyncRemoteShellArgument names OpenSSH for rsync's -e through the MSYS2/Cygwin bash beside rsync: rsync talks to its
// remote shell over a Cygwin socketpair, which the native Windows ssh.exe cannot read (the target got 0 bytes and
// rsync died with "connection unexpectedly closed"). Two cats turn the sockets into pipes ssh.exe can use; pipefail
// keeps ssh's exit status. The input cat is a process substitution so the wrapper ends when ssh.exe does: rsync keeps
// its end of the socket open until the shell exits, and a waited-for cat would never see EOF.
// rsync splits the command on spaces and keeps a quoted argument whole, a quote inside doubled.
func rsyncRemoteShellArgument(bash, ssh string) string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	return "--rsh=" + quote(bash) + ` -c 'set -o pipefail; "$0" "$@" < <(cat) | cat' ` + quote(ssh)
}

// withDefaultEnvironment adds key=value unless the environment already sets key (any case, as on Windows).
func withDefaultEnvironment(environment []string, key, value string) []string {
	for _, entry := range environment {
		if name, _, ok := strings.Cut(entry, "="); ok && strings.EqualFold(name, key) {
			return environment
		}
	}
	return append(append([]string(nil), environment...), key+"="+value)
}

// withEnvironment sets key=value, replacing any entry for key (any case, as on Windows).
func withEnvironment(environment []string, key, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if name, _, ok := strings.Cut(entry, "="); ok && strings.EqualFold(name, key) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, key+"="+value)
}
