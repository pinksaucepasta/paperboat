package machineservices

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"
)

const MaximumExplicitServices = 64
const explicitProbeTimeout = 300 * time.Millisecond
const explicitProbeConcurrency = 4

var ErrExplicitServicesInvalid = errors.New("invalid explicitly authorized machine services")

// ProbeExplicit observes only authoritative, explicitly authorized loopback
// targets. Unlike automatic socket discovery, an explicit service may belong to
// another OS user or use Docker forwarding without a host LISTEN socket.
func ProbeExplicit(ctx context.Context, services []Service) ([]Service, error) {
	return probeExplicit(ctx, services, (&net.Dialer{}).DialContext)
}

func probeExplicit(ctx context.Context, services []Service, dial func(context.Context, string, string) (net.Conn, error)) ([]Service, error) {
	if ctx == nil || len(services) > MaximumExplicitServices {
		return nil, ErrExplicitServicesInvalid
	}
	for _, service := range services {
		if service.Port == 0 || (service.Loopback != "127.0.0.1" && service.Loopback != "::1") {
			return nil, ErrExplicitServicesInvalid
		}
	}
	services = normalize(services)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	jobs := make(chan Service, len(services))
	results := make(chan Service, len(services))
	for _, service := range services {
		jobs <- service
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(explicitProbeConcurrency, len(services)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for service := range jobs {
				if ctx.Err() != nil {
					return
				}
				probeCtx, cancel := context.WithTimeout(ctx, explicitProbeTimeout)
				conn, err := dial(probeCtx, "tcp", net.JoinHostPort(service.Loopback, strconv.Itoa(int(service.Port))))
				cancel()
				if err == nil {
					_ = conn.Close()
					results <- service
				}
			}
		}()
	}
	workers.Wait()
	close(results)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	observed := make([]Service, 0, len(services))
	for service := range results {
		observed = append(observed, service)
	}
	return normalize(observed), nil
}
