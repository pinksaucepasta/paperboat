package server

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/admission"
)

type httpAdmissionKey struct{}

// HTTP requests on one admitted native connection retain its verified transfer
// authority. Resource ownership is still checked by each application handler;
// another connection or bearer must pass admission independently.
type httpAdmission struct {
	mu            sync.Mutex
	ctx           context.Context
	connection    net.Conn
	token         string
	authorization *Authorization
	release       func()
	lifetime      *admission.Lease
	stop          func() bool
}

func (a *httpAdmission) keep(token string, authorization Authorization, release func()) error {
	lease, err := admission.Admit(a.ctx, time.Time{}, authorization.RevokedSignal)
	if err != nil {
		return err
	}
	a.token = token
	a.authorization = &authorization
	a.release = release
	a.lifetime = lease
	a.stop = lease.Bind(a.connection)
	return nil
}
func (a *httpAdmission) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop != nil {
		a.stop()
	}
	if a.lifetime != nil {
		a.lifetime.Close()
	}
	if a.release != nil {
		a.release()
		a.release = nil
	}
}
