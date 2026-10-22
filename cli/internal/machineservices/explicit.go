package machineservices

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/neterr"
	"net"
	"reflect"
	"strconv"
	"sync"
	"syscall"
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
		return nil, errors.Join(err, context.Cause(ctx))
	}
	jobs := make(chan Service, len(services))
	results := make(chan Service, len(services))
	failures := make(chan error, len(services))
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
				if err != nil {
					if conn != nil {
						err = errors.Join(err, conn.Close())
					}
					if !explicitProbeAbsence(err) {
						failures <- err
					}
					continue
				}
				if conn == nil {
					failures <- errors.New("explicit machine service probe returned no connection")
					continue
				}
				if err := conn.Close(); err != nil {
					failures <- err
					continue
				}
				results <- service
			}
		}()
	}
	workers.Wait()
	close(results)
	close(failures)
	var probeFailures []error
	for err := range failures {
		probeFailures = append(probeFailures, err)
	}
	failure := errors.Join(probeFailures...)
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, context.Cause(ctx), failure)
	}
	if failure != nil {
		return nil, failure
	}
	observed := make([]Service, 0, len(services))
	for service := range results {
		observed = append(observed, service)
	}
	return normalize(observed), nil
}

// explicitProbeAbsence suppresses only a complete, bounded tree of ordinary
// loopback refusal or a target-local deadline. Mixed failures remain visible.
func explicitProbeAbsence(err error) (expected bool) {
	defer func() {
		if recover() != nil {
			expected = false
		}
	}()
	if err == nil {
		return false
	}
	pending := []error{err}
	terminals := 0
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		if value, ok := current.(interface{ Unwrap() []error }); ok {
			children := value.Unwrap()
			if len(children) == 0 || len(children) > 16-visited {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if value, ok := current.(interface{ Unwrap() error }); ok {
			child := value.Unwrap()
			if child == nil {
				return false
			}
			pending = append(pending, child)
			continue
		}
		if errno, ok := current.(syscall.Errno); ok && neterr.Refused(errno) {
			terminals++
			continue
		}
		currentType := reflect.TypeOf(current)
		deadlineType := reflect.TypeOf(context.DeadlineExceeded)
		if currentType == deadlineType && currentType.Comparable() && current == context.DeadlineExceeded {
			terminals++
			continue
		}
		return false
	}
	return terminals > 0
}
