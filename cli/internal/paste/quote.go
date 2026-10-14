package paste

import (
	"errors"
	"strings"
)

// quoteRemotePath uses the destination path format, never the client OS.
// Windows attach supports cmd and PowerShell without declaring the shell dialect.
// Double quotes work in both for ordinary paths, but expansion-sensitive names
// cannot be represented portably; retain the original paste visibly in that case.
func quoteRemotePath(path string) (string, error) {
	windows := len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/') || strings.HasPrefix(path, `\\`)
	if strings.ContainsAny(path, "\x00\r\n") || (!windows && !strings.HasPrefix(path, "/")) {
		return "", errors.New("uploaded path is not a valid absolute terminal path")
	}
	if windows && strings.ContainsAny(path, "\"$`%!^") {
		return "", errors.New("uploaded path requires shell-specific quoting; original paste was kept")
	}
	safe := true
	for _, r := range path {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/_-.:", r) || windows && r == '\\' {
			continue
		}
		safe = false
		break
	}
	if safe {
		return path, nil
	}
	if windows {
		return "\"" + path + "\"", nil
	}
	return "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'", nil
}
