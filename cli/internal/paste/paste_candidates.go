package paste

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type pathCandidate struct {
	path       string
	file       *os.File
	start, end int
}

// parseCandidate accepts exactly one POSIX shell word with optional surrounding
// whitespace. It deliberately decodes only syntax that has one unambiguous
// literal value, rejecting operators, expansions, and malformed quoting.
// start/end identify the complete source word so staged paths replace any
// source quoting; uploaded paths are quoted separately for literal insertion.
func parseCandidate(line string) (pathCandidate, bool) {
	trimmedLeft := strings.TrimLeft(line, " \t\r")
	start := len(line) - len(trimmedLeft)
	trimmed := strings.TrimRight(trimmedLeft, " \t\r")
	end := start + len(trimmed)
	if start == end {
		return pathCandidate{}, false
	}
	var localPath string
	var hasUnquotedGlob, ok bool
	if runtime.GOOS == "windows" && strings.HasPrefix(strings.ToLower(trimmed), "file://") {
		// Keep backslashes in legacy Windows file URIs intact. POSIX shell
		// decoding would incorrectly consume them before URI handling.
		localPath, ok = trimmed, true
	} else {
		localPath, hasUnquotedGlob, ok = decodeCandidateWord(trimmed)
	}
	if !ok || localPath == "" {
		return pathCandidate{}, false
	}
	if strings.HasPrefix(strings.ToLower(localPath), "file:") {
		if runtime.GOOS == "windows" {
			// Terminal applications commonly emit the legacy Windows spelling
			// file://C:\\path or file://localhostC:\\path rather than a
			// standards-compliant file:///C:/path URI. Accept only those
			// unambiguous local-drive forms.
			raw := localPath[len("file://"):]
			if strings.HasPrefix(strings.ToLower(raw), "localhost") {
				raw = raw[len("localhost"):]
			}
			if len(raw) >= 2 && raw[1] == ':' && ((raw[0] >= 'a' && raw[0] <= 'z') || (raw[0] >= 'A' && raw[0] <= 'Z')) {
				decoded, decodeErr := url.PathUnescape(raw)
				if decodeErr != nil {
					return pathCandidate{}, false
				}
				localPath = decoded
				goto filePathDecoded
			}
		}
		u, err := url.Parse(localPath)
		if err != nil || !strings.EqualFold(u.Scheme, "file") ||
			(u.Host != "" && !strings.EqualFold(u.Host, "localhost")) ||
			u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
			return pathCandidate{}, false
		}
		localPath = u.Path
		if runtime.GOOS == "windows" && len(localPath) >= 3 && localPath[0] == '/' && localPath[2] == ':' {
			localPath = localPath[1:]
		}
	} else if hasUnquotedGlob {
		return pathCandidate{}, false
	}
filePathDecoded:
	localPath = filepath.FromSlash(localPath)
	if !filepath.IsAbs(localPath) {
		return pathCandidate{}, false
	}
	return pathCandidate{path: localPath, start: start, end: end}, true
}

func decodeCandidateWord(word string) (string, bool, bool) {
	if runtime.GOOS == "windows" {
		candidate := word
		if len(candidate) >= 2 && (candidate[0] == '"' && candidate[len(candidate)-1] == '"' || candidate[0] == '\'' && candidate[len(candidate)-1] == '\'') {
			candidate = candidate[1 : len(candidate)-1]
		}
		if strings.Contains(candidate, `\ `) && filepath.IsAbs(filepath.FromSlash(candidate)) {
			return strings.ReplaceAll(candidate, `\ `, " "), false, true
		}
		if !strings.ContainsAny(candidate, "\x00\r\n") && filepath.IsAbs(filepath.FromSlash(candidate)) {
			return candidate, false, true
		}
	}
	return decodePOSIXWord(word)
}

// decodePOSIXWord decodes one shell word without invoking a shell. The return
// value records unquoted glob characters, which are only allowed for file URIs
// long enough to reject their query/fragment syntax separately.
func decodePOSIXWord(word string) (decoded string, hasUnquotedGlob bool, ok bool) {
	var out strings.Builder
	for pos := 0; pos < len(word); {
		switch word[pos] {
		case '\'', '"':
			quote := word[pos]
			pos++
			closed := false
			for pos < len(word) {
				ch := word[pos]
				if ch == quote {
					pos++
					closed = true
					break
				}
				if quote == '"' && (ch == '$' || ch == '`') {
					return "", false, false
				}
				if ch == '\\' {
					if pos+1 >= len(word) || word[pos+1] == '\n' {
						return "", false, false
					}
					if quote == '"' && !strings.ContainsRune("$`\"\\", rune(word[pos+1])) {
						out.WriteByte(ch)
						pos++
						continue
					}
					pos++
					out.WriteByte(word[pos])
					pos++
					continue
				}
				out.WriteByte(ch)
				pos++
			}
			if !closed {
				return "", false, false
			}
		case '\\':
			if pos+1 >= len(word) || word[pos+1] == '\n' {
				return "", false, false
			}
			pos++
			out.WriteByte(word[pos])
			pos++
		case ' ', '\t', '\r', '\n', '$', '`', '|', '&', ';', '<', '>', '(', ')', '{', '}', '~':
			return "", false, false
		case '*', '?', '[':
			hasUnquotedGlob = true
			out.WriteByte(word[pos])
			pos++
		default:
			out.WriteByte(word[pos])
			pos++
		}
	}
	return out.String(), hasUnquotedGlob, true
}

// partialSuffix returns the length of the longest suffix of buf that is a
// proper prefix of marker (so those bytes are withheld until the next Write).
func partialSuffix(buf, marker []byte) int {
	max := len(marker) - 1
	if len(buf) < max {
		max = len(buf)
	}
	for n := max; n > 0; n-- {
		if bytes.Equal(buf[len(buf)-n:], marker[:n]) {
			return n
		}
	}
	return 0
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}
