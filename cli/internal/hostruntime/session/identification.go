package session

import (
	"net/url"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// identificationTracker observes output without consuming or rewriting any PTY
// bytes. Metadata is untrusted display text, never authority or progress evidence.
type identificationTracker struct {
	title     string
	directory string
	state     byte
	overflow  bool
	payload   []byte
}

const maxIdentificationSequence = 8192
const (
	identificationGround byte = iota
	identificationEscape
	identificationOSC
	identificationOSCEscape
	identificationString
	identificationStringEscape
)

func (t *identificationTracker) Consume(data []byte) {
	for _, b := range data {
		switch t.state {
		case identificationGround:
			if b == 0x1b {
				t.state = identificationEscape
			}
		case identificationEscape:
			switch b {
			case ']':
				t.state = identificationOSC
				t.payload = t.payload[:0]
				t.overflow = false
			case 'P', '_', '^', 'X':
				t.state = identificationString
			case 0x1b: // remain in escape state
			default:
				t.state = identificationGround
			}
		case identificationOSC:
			switch b {
			case 7:
				t.finish()
			case 0x18, 0x1a:
				t.state = identificationGround
				t.payload = t.payload[:0]
			case 0x1b:
				t.state = identificationOSCEscape
			default:
				t.append(b)
			}
		case identificationOSCEscape:
			if b == '\\' {
				t.finish()
			} else {
				// An embedded escape makes this OSC invalid. Drain it to its terminator
				// rather than treating its contents as another metadata instruction.
				t.overflow = true
				if b == 7 {
					t.finish()
				} else if b != 0x1b {
					t.state = identificationOSC
				}
			}
		case identificationString:
			if b == 0x1b {
				t.state = identificationStringEscape
			} else if b == 0x18 || b == 0x1a {
				t.state = identificationGround
			}
		case identificationStringEscape:
			if b == '\\' || b == 0x18 || b == 0x1a {
				t.state = identificationGround
			} else if b != 0x1b {
				t.state = identificationString
			}
		}
	}
}

func (t *identificationTracker) append(b byte) {
	if t.overflow {
		return
	}
	if len(t.payload) == maxIdentificationSequence {
		t.overflow = true
		t.payload = t.payload[:0]
		return
	}
	t.payload = append(t.payload, b)
}

func (t *identificationTracker) finish() {
	t.state = identificationGround
	if !t.overflow && utf8.Valid(t.payload) {
		code, value, ok := strings.Cut(string(t.payload), ";")
		if ok {
			switch code {
			case "0", "2":
				t.title = cleanIdentification(value, 128)
			case "9":
				if path, ok := strings.CutPrefix(value, "9;"); ok && (path == "" || windowsAbsoluteDirectory(path)) {
					t.directory = cleanIdentification(strings.Trim(path, "\""), 1024)
				}
			case "7":
				u, err := url.Parse(value)
				if err == nil && strings.EqualFold(u.Scheme, "file") && u.User == nil && u.Port() == "" && u.RawQuery == "" && u.Fragment == "" && u.Opaque == "" && strings.HasPrefix(u.Path, "/") {
					path := u.Path
					if runtime.GOOS == "windows" {
						if u.Host != "" {
							path = "//" + u.Host + path
						} else if windowsAbsoluteDirectory(strings.TrimPrefix(path, "/")) {
							path = strings.TrimPrefix(path, "/")
						}
					}
					t.directory = cleanIdentification(path, 1024)
				}
			}
		}
	}
	t.payload = t.payload[:0]
}

func cleanIdentification(value string, maximum int) string {
	out := make([]rune, 0, min(utf8.RuneCountInString(value), maximum))
	for _, r := range value {
		if r != utf8.RuneError && !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && len(out) < maximum {
			out = append(out, r)
		}
	}
	return strings.TrimSpace(string(out))
}

// OSC 9;9 is the Windows Terminal current-directory convention. Do not use
// filepath.IsAbs here: a Linux control-plane build must preserve Windows paths.
func windowsAbsoluteDirectory(value string) bool {
	value = strings.Trim(value, "\"")
	return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && (value[2] == '\\' || value[2] == '/') || strings.HasPrefix(value, `\\`)
}
