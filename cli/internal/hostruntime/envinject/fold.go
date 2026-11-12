package envinject

import (
	"runtime"
	"strings"
)

// CaseInsensitiveNames is true where the OS treats Path and PATH as one variable (Windows); Linux and macOS keep
// names case-sensitive. Tests flip it to check both rules on any host.
var CaseInsensitiveNames = runtime.GOOS == "windows"

// FoldName is the key under which two environment names are the same variable on this OS.
func FoldName(name string) string {
	if CaseInsensitiveNames {
		return strings.ToUpper(name)
	}
	return name
}
