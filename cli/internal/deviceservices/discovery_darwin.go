//go:build darwin

package deviceservices

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
	return services, nil
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
