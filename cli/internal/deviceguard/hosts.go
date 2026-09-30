package deviceguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strings"
)

const hostsStart = "# PaperboatHostsSectionStart"
const hostsEnd = "# PaperboatHostsSectionEnd"
const maxHostsSize = 1 << 20

// renderDeviceHosts preserves every byte outside the single owned section.
// Only native device names belong here; browser port names use public DNS.
func renderDeviceHosts(original []byte, names map[string]string) ([]byte, error) {
	if len(original) > maxHostsSize || bytes.ContainsRune(original, 0) {
		return nil, errors.New("hosts file is too large or has unsupported encoding; left unchanged")
	}
	keys := make([]string, 0, len(names))
	for name, address := range names {
		ip, err := netip.ParseAddr(address)
		if !validName(name) || len(strings.Split(name, ".")) != 2 || err != nil || !ip.Is4() || !ip.IsLoopback() {
			return nil, errors.New("invalid native device hosts mapping")
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
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
				if len(fields) != 2 || !validName(fields[1]) || len(strings.Split(fields[1], ".")) != 2 {
					return nil, errors.New("unexpected content in owned hosts section; left unchanged")
				}
				ip, err := netip.ParseAddr(fields[0])
				if err != nil || !ip.Is4() || !ip.IsLoopback() {
					return nil, errors.New("invalid address in owned hosts section; left unchanged")
				}
			} else {
				for _, name := range fields {
					if _, ok := names[strings.ToLower(name)]; ok {
						return nil, fmt.Errorf("device hostname %s already exists outside Paperboat's hosts section; left unchanged", name)
					}
				}
				outside.Write(line)
			}
		}
	}
	if inside {
		return nil, errors.New("unterminated hosts section; left unchanged")
	}
	if len(keys) == 0 {
		return outside.Bytes(), nil
	}
	var out bytes.Buffer
	remainder := outside.Bytes()
	if bytes.HasPrefix(remainder, []byte("\ufeff")) {
		out.WriteString("\ufeff")
		remainder = bytes.TrimPrefix(remainder, []byte("\ufeff"))
	}
	newline := "\n"
	if bytes.Contains(original, []byte("\r\n")) {
		newline = "\r\n"
	}
	out.WriteString(hostsStart + newline)
	for _, name := range keys {
		fmt.Fprintf(&out, "%s %s%s", names[name], name, newline)
	}
	out.WriteString(hostsEnd + newline)
	out.Write(remainder)
	if out.Len() > maxHostsSize {
		return nil, errors.New("device hosts projection exceeds size limit; left unchanged")
	}
	return out.Bytes(), nil
}

func updateDeviceHosts(ctx context.Context, path string, names map[string]string) error {
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
	next, err := renderDeviceHosts(original, names)
	if err != nil {
		return err
	}
	if bytes.Equal(original, next) {
		return nil
	}
	return replaceHostsFile(ctx, path, info, original, next)
}

func projectDeviceHosts(ctx context.Context, cfg Config, names map[string]string) error {
	path, err := systemHostsPath()
	if cfg.hostsPath != "" {
		path, err = cfg.hostsPath, nil
	}
	if err != nil {
		return err
	}
	if err = updateDeviceHosts(ctx, path, names); err != nil {
		return fmt.Errorf("update native device hosts mappings: %w", err)
	}
	flushHostsCache()
	return nil
}
