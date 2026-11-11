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

// rsyncRemoteShellChosen reports whether the user picked rsync's remote shell (-e, --rsh, RSYNC_RSH); pb then
// leaves it alone.
func rsyncRemoteShellChosen(args, environment []string) bool {
	for _, entry := range environment {
		if key, _, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, "RSYNC_RSH") {
			return true
		}
	}
	for _, value := range args {
		if value == "--" {
			return false
		}
		if value == "--rsh" || strings.HasPrefix(value, "--rsh=") {
			return true
		}
		if strings.HasPrefix(value, "-") && !strings.HasPrefix(value, "--") {
			// In a short cluster the first value option (-e, -B, -f) takes the rest: "-ave ssh", not "-fe".
			if index := strings.IndexAny(value[1:], "eBf"); index >= 0 && value[1+index] == 'e' {
				return true
			}
		}
	}
	return false
}

// rsyncRemoteShellArgument names OpenSSH for rsync's -e: rsync splits the command on spaces and keeps a quoted
// argument whole, a quote inside doubled.
func rsyncRemoteShellArgument(ssh string) string {
	return "--rsh='" + strings.ReplaceAll(ssh, "'", "''") + "'"
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
