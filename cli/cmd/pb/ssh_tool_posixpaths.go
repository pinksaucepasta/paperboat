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
