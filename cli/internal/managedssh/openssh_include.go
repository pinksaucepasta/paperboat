package managedssh

import (
	"bytes"
	"strings"
)

// OpenSSHFirstBlockOffset is where the first Host or Match block of an OpenSSH client configuration starts. OpenSSH
// applies an Include below it only to the hosts of its block.
func OpenSSHFirstBlockOffset(value []byte) int {
	offset := 0
	for _, line := range bytes.SplitAfter(value, []byte{'\n'}) {
		// OpenSSH also accepts "Host=name".
		keyword := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(line)), "=", 2)[0])
		if fields := strings.Fields(keyword); len(fields) > 0 && (strings.EqualFold(fields[0], "Host") || strings.EqualFold(fields[0], "Match")) {
			return offset
		}
		offset += len(line)
	}
	return len(value)
}

// findOpenSSHInclude returns pb's Include line in main, with its line end. Commented means the user disabled it.
func findOpenSSHInclude(main []byte, includeLine string) (start, end int, commented, found bool) {
	text := strings.TrimSpace(includeLine)
	offset := 0
	for _, line := range bytes.SplitAfter(main, []byte{'\n'}) {
		trimmed := strings.TrimSpace(string(line))
		if strings.TrimLeft(trimmed, "# \t") == text {
			if found {
				return 0, 0, false, false
			}
			start, end, commented, found = offset, offset+len(line), trimmed != text, true
		}
		offset += len(line)
	}
	return start, end, commented, found
}

func insertOpenSSHInclude(main []byte, includeLine string) (next []byte, chunk string) {
	offset := OpenSSHFirstBlockOffset(main)
	chunk = includeLine
	if offset > 0 && main[offset-1] != '\n' {
		chunk = "\n" + chunk
	}
	return []byte(string(main[:offset]) + chunk + string(main[offset:])), chunk
}

// openSSHIncludeInPlace: pb's Include line applies to every host and still is what pb recorded.
func openSSHIncludeInPlace(main []byte, chunk, includeLine string) bool {
	start, _, commented, found := findOpenSSHInclude(main, includeLine)
	return found && !commented && start < OpenSSHFirstBlockOffset(main) && bytes.Count(main, []byte(chunk)) == 1
}

// repairOpenSSHInclude moves an Include line that was moved into a Host or Match block back above the blocks. A
// commented-out line is the user's choice and stays.
func repairOpenSSHInclude(main []byte, chunk, includeLine string) (next []byte, nextChunk string, ok bool) {
	start, end, commented, found := findOpenSSHInclude(main, includeLine)
	if !found || commented || openSSHIncludeInPlace(main, chunk, includeLine) {
		return main, chunk, found
	}
	next, nextChunk = insertOpenSSHInclude([]byte(string(main[:start])+string(main[end:])), includeLine)
	return next, nextChunk, true
}

// removeOpenSSHInclude takes out exactly what pb inserted, or the whole line when the user moved or disabled it.
func removeOpenSSHInclude(main []byte, chunk, includeLine string) (next []byte, ok bool) {
	if openSSHIncludeInPlace(main, chunk, includeLine) {
		return bytes.Replace(main, []byte(chunk), nil, 1), true
	}
	start, end, _, found := findOpenSSHInclude(main, includeLine)
	if !found {
		return nil, false
	}
	return []byte(string(main[:start]) + string(main[end:])), true
}
