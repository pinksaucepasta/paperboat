package machineguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
)

const hostsStart = "# PaperboatHostsSectionStart"
const hostsEnd = "# PaperboatHostsSectionEnd"
const maxHostsSize = 1 << 20

// renderMachineHosts removes only a prior marker-owned hosts section.
func renderMachineHosts(original []byte) ([]byte, error) {
	if len(original) > maxHostsSize || bytes.ContainsRune(original, 0) {
		return nil, errors.New("hosts file is too large or has unsupported encoding; left unchanged")
	}
	var outside bytes.Buffer
	inside, found := false, false
	for _, line := range bytes.SplitAfter(original, []byte("\n")) {
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		// A BOM is preserved at the beginning, outside our section.
		if outside.Len() == 0 && !found && strings.HasPrefix(text, "\ufeff") {
			outside.WriteString("\ufeff")
			line = bytes.TrimPrefix(line, []byte("\ufeff"))
			text = strings.TrimPrefix(text, "\ufeff")
		}
		switch text {
		case hostsStart:
			if inside || found {
				return nil, errors.New("duplicate hosts section; left unchanged")
			}
			inside, found = true, true
		case hostsEnd:
			if !inside {
				return nil, errors.New("unmatched hosts section marker; left unchanged")
			}
			inside = false
		default:
			if strings.Contains(text, hostsStart) || strings.Contains(text, hostsEnd) {
				return nil, errors.New("malformed hosts section marker; left unchanged")
			}
			fields := strings.Fields(strings.SplitN(text, "#", 2)[0])
			if inside {
				if len(fields) == 0 {
					continue
				}
				if len(fields) != 2 || !validOwnedMachineHost(fields[1]) {
					return nil, errors.New("unexpected content in owned hosts section; left unchanged")
				}
				ip, err := netip.ParseAddr(fields[0])
				if err != nil || !ip.Is4() || !ip.IsLoopback() {
					return nil, errors.New("invalid address in owned hosts section; left unchanged")
				}
			} else {
				outside.Write(line)
			}
		}
	}
	if inside {
		return nil, errors.New("unterminated hosts section; left unchanged")
	}
	return outside.Bytes(), nil
}

// validOwnedMachineHost accepts only formats emitted by the previous host
// projections. The old flat .pprbt names remain parseable solely so their
// marked block can be removed without touching administrator-owned entries.
func validOwnedMachineHost(name string) bool {
	if validName(name) {
		return true
	}
	if !strings.HasSuffix(name, ".pprbt") || name != strings.ToLower(name) {
		return false
	}
	label := strings.TrimSuffix(name, ".pprbt")
	if len(label) < 1 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return false
	}
	for _, r := range label {
		if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func removeOwnedMachineHosts(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("hosts file must be a regular file; left unchanged")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	original, err := io.ReadAll(io.LimitReader(file, maxHostsSize+1))
	file.Close()
	if err != nil {
		return err
	}
	next, err := renderMachineHosts(original)
	if err != nil {
		return err
	}
	if bytes.Equal(original, next) {
		return nil
	}
	return replaceHostsFile(ctx, path, info, original, next)
}

func clearMachineHosts(ctx context.Context, cfg Config) error {
	path, err := systemHostsPath()
	if cfg.hostsPath != "" {
		path, err = cfg.hostsPath, nil
	}
	if err != nil {
		return err
	}
	if err = removeOwnedMachineHosts(ctx, path); err != nil {
		return fmt.Errorf("remove owned machine hosts mappings: %w", err)
	}
	flushHostsCache()
	return nil
}
