//go:build linux

package deviceservices

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
)

func snapshot(ctx context.Context) ([]Service, error) {
	remaining := int64(maximumSnapshotBytes)
	var services []Service
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		data, readErr := readBounded(file, remaining)
		closeErr := file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		remaining -= int64(len(data))
		services = append(services, parseProcTCP(data, strings.HasSuffix(path, "tcp6"), uint64(os.Geteuid()))...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return services, nil
}

func parseProcTCP(data []byte, ipv6 bool, ownerUID uint64) []Service {
	var services []Service
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[3] != "0A" {
			continue
		}
		uid, err := strconv.ParseUint(fields[7], 10, 32)
		if err != nil || uid != ownerUID {
			continue
		}
		address, portText, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		portValue, err := strconv.ParseUint(portText, 16, 16)
		if err != nil || portValue == 0 {
			continue
		}
		loopback := ""
		if ipv6 {
			decoded, err := hex.DecodeString(address)
			if err != nil || len(decoded) != 16 {
				continue
			}
			switch {
			case bytes.Equal(decoded, make([]byte, 16)):
				loopback = "::1"
			case bytes.Equal(decoded, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0}):
				loopback = "::1"
			}
		} else {
			switch {
			case address == "00000000":
				loopback = "127.0.0.1"
			case address == "0100007F":
				loopback = "127.0.0.1"
			}
		}
		if loopback != "" {
			services = append(services, Service{Port: uint16(portValue), Loopback: loopback})
		}
	}
	return services
}
