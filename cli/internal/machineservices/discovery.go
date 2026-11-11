// Package machineservices discovers local TCP services eligible for private
// machine-service announcement. Automatic discovery observes socket tables
// without probing ports or reporting process metadata. Explicit publications
// may check only their exact authorized loopback targets.
package machineservices

import (
	"context"
	"errors"
	"io"
	"sort"
)

const maximumSnapshotBytes = 4 << 20

var (
	ErrDiscoveryUnavailable = errors.New("machine service discovery unavailable")
	ErrSnapshotTooLarge     = errors.New("machine service socket table exceeds 4 MiB")
)

type Service struct {
	Port     uint16
	Loopback string
}

func Snapshot(ctx context.Context) ([]Service, error) {
	if ctx == nil {
		return nil, ErrDiscoveryUnavailable
	}
	services, err := snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return normalize(services), nil
}

func normalize(in []Service) []Service {
	seen := make(map[Service]struct{}, len(in))
	out := make([]Service, 0, len(in))
	for _, service := range in {
		if service.Port == 0 || service.Loopback != "127.0.0.1" && service.Loopback != "::1" {
			continue
		}
		if _, ok := seen[service]; ok {
			continue
		}
		seen[service] = struct{}{}
		out = append(out, service)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Loopback < out[j].Loopback
	})
	return out
}

// readBufferBytes is the first read buffer. A /proc/net/tcp read returns whole lines up to the buffer's size and every
// read call walks the kernel's socket table again, so io.ReadAll's 512-byte start cost a syscall per three lines: that
// was 85% of an idle worker's CPU (pprof, 2026-11-11), re-read every two seconds.
const readBufferBytes = 64 << 10

func readBounded(r io.Reader, remaining int64) ([]byte, error) {
	if remaining < 0 {
		return nil, ErrSnapshotTooLarge
	}
	limited := io.LimitReader(r, remaining+1)
	data := make([]byte, 0, min(remaining+1, readBufferBytes))
	for {
		if len(data) == cap(data) {
			data = append(data, 0)[:len(data)]
		}
		n, err := limited.Read(data[len(data):cap(data)])
		data = data[:len(data)+n]
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if int64(len(data)) > remaining {
		return nil, ErrSnapshotTooLarge
	}
	return data, nil
}
