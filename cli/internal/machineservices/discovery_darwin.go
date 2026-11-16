//go:build darwin

package machineservices

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func snapshot(ctx context.Context) ([]Service, error) {
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	uid := uint64(os.Geteuid())
	remaining := maximumSnapshotBytes
	var services []Service
	for _, family := range []struct{ flag, loopback string }{{"-i4TCP", "127.0.0.1"}, {"-i6TCP", "::1"}} {
		var output boundedBuffer
		output.limit = remaining
		cmd := exec.CommandContext(runCtx, "/usr/sbin/lsof", "-nP", "-a", "-u", strconv.FormatUint(uid, 10), family.flag, "-sTCP:LISTEN", "-FpuPn")
		cmd.Stdout, cmd.Stderr = &output, &output
		err := cmd.Run()
		if errors.Is(output.err, ErrSnapshotTooLarge) {
			return nil, output.err
		}
		if runCtx.Err() != nil {
			return nil, runCtx.Err()
		}
		var exitErr *exec.ExitError
		if err != nil && !(errors.As(err, &exitErr) && output.Len() == 0) {
			return nil, errors.Join(ErrDiscoveryUnavailable, err)
		}
		remaining -= output.Len()
		services = append(services, parseLsof(output.Bytes(), uid, family.loopback)...)
	}
	// lsof shows only this user's sockets; port 80 and 443 listeners of any owner come from the socket table.
	var table boundedBuffer
	table.limit = remaining
	cmd := exec.CommandContext(runCtx, "/usr/sbin/netstat", "-an", "-p", "tcp")
	cmd.Stdout, cmd.Stderr = &table, &table
	if err := cmd.Run(); errors.Is(table.err, ErrSnapshotTooLarge) {
		return nil, table.err
	} else if runCtx.Err() != nil {
		return nil, runCtx.Err()
	} else if err != nil {
		return nil, errors.Join(ErrDiscoveryUnavailable, err)
	}
	return append(services, parseNetstatWebPorts(table.Bytes())...), nil
}

// parseNetstatWebPorts lists the wildcard or loopback listeners on port 80 and 443 in `netstat -an -p tcp` output:
// "tcp4  0  0  *.443  *.*  LISTEN" (tcp46 listens on both families).
func parseNetstatWebPorts(data []byte) []Service {
	var services []Service
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || !strings.HasPrefix(fields[0], "tcp") || fields[len(fields)-1] != "LISTEN" {
			continue
		}
		separator := strings.LastIndex(fields[3], ".")
		if separator < 0 {
			continue
		}
		host := fields[3][:separator]
		port, err := strconv.ParseUint(fields[3][separator+1:], 10, 16)
		if err != nil || !isWebPort(uint16(port)) {
			continue
		}
		if fields[0] != "tcp6" && (host == "*" || host == "127.0.0.1") {
			services = append(services, Service{Port: uint16(port), Loopback: "127.0.0.1"})
		}
		if fields[0] != "tcp4" && (host == "*" || host == "::1") {
			services = append(services, Service{Port: uint16(port), Loopback: "::1"})
		}
	}
	return services
}

type boundedBuffer struct {
	bytes.Buffer
	err   error
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.err = ErrSnapshotTooLarge
		return 0, b.err
	}
	return b.Buffer.Write(p)
}

func parseLsof(data []byte, ownerUID uint64, loopback string) []Service {
	var services []Service
	owned := false
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			owned = false
		case 'u':
			uid, err := strconv.ParseUint(line[1:], 10, 32)
			owned = err == nil && uid == ownerUID
		case 'n':
			if !owned {
				continue
			}
			name := strings.TrimSuffix(line[1:], " (LISTEN)")
			separator := strings.LastIndex(name, ":")
			if separator < 0 {
				continue
			}
			host := name[:separator]
			port, err := strconv.ParseUint(name[separator+1:], 10, 16)
			if err != nil || port == 0 {
				continue
			}
			if loopback == "127.0.0.1" && host != "*" && host != "0.0.0.0" && host != "127.0.0.1" || loopback == "::1" && host != "*" && host != "[::]" && host != "[::1]" {
				continue
			}
			services = append(services, Service{Port: uint16(port), Loopback: loopback})
		}
	}
	return services
}
