// Package transfername decides whether a machine of a given platform can store a received file under a name.
//
// A sender checks the names before it sends so the user is told which file and why; the receiver checks them again when
// it admits the batch, so no other client (console, SDK, team request) can make it fail halfway or rename silently.
// Prior art: Taildrop's validateBaseName (feature/taildrop/taildrop.go in ~/ref/tailscale) rejects separators, Windows-invalid
// characters, surrogates and non-graphic runes, and surrounding white space; this package adds the reserved Windows device
// names and the trailing dot, and applies the Windows rules only to a Windows receiver.
package transfername

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxBytes is the longest file name every supported file system stores.
const MaxBytes = 255

var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}

// Refusal returns why a receiver on platform ("windows", "darwin" or "linux") cannot store name, or "" when it can.
func Refusal(platform, name string) string {
	switch {
	case name == "":
		return "the name is empty"
	case !utf8.ValidString(name):
		return "the name is not valid UTF-8"
	case len(name) > MaxBytes:
		return fmt.Sprintf("the name is %d bytes long; file systems store at most %d", len(name), MaxBytes)
	case name == "." || name == "..":
		return "the name is a directory reference"
	}
	if i := strings.IndexAny(name, "/\x00"); i >= 0 {
		return fmt.Sprintf("the name contains %q, which no file system allows", name[i])
	}
	if platform == "windows" {
		return windowsRefusal(name)
	}
	return ""
}

func windowsRefusal(name string) string {
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Sprintf("the name contains the control character U+%04X, which Windows does not allow", r)
		}
	}
	if i := strings.IndexAny(name, `\:*?"<>|`); i >= 0 {
		return fmt.Sprintf("the name contains %q, which Windows does not allow in file names", name[i])
	}
	if last := name[len(name)-1]; last == ' ' || last == '.' {
		return fmt.Sprintf("the name ends with %q, which Windows silently removes", last)
	}
	stem := name
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		stem = name[:dot]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	if windowsReserved[stem] || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
		return fmt.Sprintf("%s is a reserved Windows device name, with or without an extension", stem)
	}
	return ""
}

// Portable is the name a browser on any OS saves a downloaded Inbox file under without changing it: the characters Windows
// does not allow become "_", a trailing dot or space becomes "_", and a reserved device name gets a leading "_" (what Chrome
// on Windows does on its own; Linux and macOS browsers keep that result as it is). The Inbox keeps the original name; only
// the download name is made portable, so the same item is saved under the same name on every OS.
func Portable(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`\:*?"<>|/`, r) {
			r = '_'
		}
		b.WriteRune(r)
	}
	out := b.String()
	if n := len(out); n > 0 && (out[n-1] == ' ' || out[n-1] == '.') {
		out = out[:n-1] + "_"
	}
	if windowsRefusal(out) != "" {
		out = "_" + out
	}
	return out
}
