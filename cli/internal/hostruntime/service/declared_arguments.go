package service

import (
	"errors"
	"os"
	"strings"

	"github.com/coreos/go-systemd/v22/unit"
	"howett.net/plist"
)

// DeclaredArguments are the program arguments (after the executable) of the service definition installed at path:
// what renderLaunchd and renderSystemd wrote. nil with no error means nothing is installed there.
func DeclaredArguments(platform, path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch platform {
	case "darwin":
		var declaration struct {
			Arguments []string `plist:"ProgramArguments"`
		}
		if _, err := plist.Unmarshal(body, &declaration); err != nil {
			return nil, err
		}
		if len(declaration.Arguments) == 0 {
			return nil, ErrInvalidDefinition
		}
		return declaration.Arguments[1:], nil
	case "linux":
		options, err := unit.DeserializeOptions(strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		for _, option := range options {
			if option.Section == "Service" && option.Name == "ExecStart" {
				tokens, ok := systemdWords(option.Value)
				if !ok || len(tokens) == 0 {
					return nil, ErrInvalidDefinition
				}
				return tokens[1:], nil
			}
		}
		return nil, ErrInvalidDefinition
	}
	return nil, ErrUnsupportedPlatform
}

// systemdWords reverses systemdEscape over a whole command line: space-separated double-quoted words.
func systemdWords(line string) ([]string, bool) {
	var words []string
	for i := 0; i < len(line); {
		if line[i] == ' ' {
			i++
			continue
		}
		if line[i] != '"' {
			return nil, false
		}
		var word strings.Builder
		i++
		closed := false
		for i < len(line) && !closed {
			switch c := line[i]; {
			case c == '"':
				closed = true
			case c == '\\' && i+1 < len(line):
				i++
				word.WriteByte(line[i])
			case (c == '%' || c == '$') && i+1 < len(line) && line[i+1] == c:
				i++
				word.WriteByte(c)
			default:
				word.WriteByte(c)
			}
			i++
		}
		if !closed {
			return nil, false
		}
		words = append(words, word.String())
	}
	return words, true
}
