// Package deviceservices discovers local TCP services eligible for private
// device-service announcement. It observes socket tables; it never probes a
// port or reports owning process metadata.
package deviceservices

import (
	"context"
	"errors"
	"io"
	"sort"
)

const maximumSnapshotBytes = 4 << 20

var (
	ErrDiscoveryUnavailable = errors.New("device service discovery unavailable")
	ErrSnapshotTooLarge     = errors.New("device service socket table exceeds 4 MiB")
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

func readBounded(r io.Reader, remaining int64) ([]byte, error) {
	if remaining < 0 {
		return nil, ErrSnapshotTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(r, remaining+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > remaining {
		return nil, ErrSnapshotTooLarge
	}
	return data, nil
}
