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

	"github.com/pinksaucepasta/paperboat/shared/boundedread"
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

// listedListener says whether discovery lists a listener: the daemon user's own, and any listener on port 80 or
// 443 whoever owns it (root, a Docker proxy, another user's Traefik or Caddy), because default private access covers
// those web ports. Other ports below 1024 of other owners are never listed; they need an explicit allow.
func listedListener(ownedByDaemonUser bool, port uint16) bool {
	return ownedByDaemonUser || isWebPort(port)
}

func isWebPort(port uint16) bool { return port == 80 || port == 443 }

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
	data, err := boundedread.All(r, remaining)
	if errors.Is(err, boundedread.ErrTooLarge) {
		return nil, ErrSnapshotTooLarge
	}
	return data, err
}

// memoizeByPID answers each process id once for the life of the returned function (one discovery pass).
func memoizeByPID(check func(uint32) bool) func(uint32) bool {
	answers := make(map[uint32]bool)
	return func(pid uint32) bool {
		if answer, known := answers[pid]; known {
			return answer
		}
		answer := check(pid)
		answers[pid] = answer
		return answer
	}
}
