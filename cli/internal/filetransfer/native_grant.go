package filetransfer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// NativeGrant couples one freshly authorized resource credential to its
// PID-bound native lease. Renewal must reauthorize the original resource.
type NativeGrant struct {
	Endpoint string
	Auth     Auth
	Binding  Binding
	Policy   Policy
	Lease    interface {
		OpenTransferStream(context.Context) (net.Conn, error)
		Close() error
	}
}

var errNativeAuthorizationChanged = errors.New("native transfer authorization changed")

type nativeGrantTransport struct {
	grant     NativeGrant
	transport http.RoundTripper
	users     int
	retired   bool
}
type nativeGrantOwner struct {
	mu         sync.Mutex
	current    *nativeGrantTransport
	grants     map[string]*nativeGrantTransport
	closed     bool
	cleanupErr error
	cleanup    sync.WaitGroup
}

// NewRenewableNativeClient owns the initial lease on success and all replacement
// leases. Close reports any deferred transport or lease cleanup failures.
func NewRenewableNativeClient(initial NativeGrant, renew func(context.Context) (NativeGrant, error)) (*NativeClient, error) {
	if initial.Lease == nil || renew == nil || initial.Auth.ExpiresAt.IsZero() || !initial.Auth.ExpiresAt.After(serverclock.Now()) {
		return nil, errors.New("invalid native transfer grant")
	}
	client, err := NewNativeClient(initial.Endpoint, initial.Auth, initial.Binding, initial.Lease.OpenTransferStream)
	if err != nil {
		return nil, err
	}
	transport := client.HTTPClient.Transport
	owner := &nativeGrantOwner{current: &nativeGrantTransport{grant: initial, transport: transport}, grants: make(map[string]*nativeGrantTransport)}
	owner.grants[initial.Auth.Token] = owner.current
	client.HTTPClient.Transport = owner
	client.RefreshAuth = func(ctx context.Context) (Auth, error) {
		fresh, err := renew(ctx)
		if err != nil && localDaemonRestarting(err) {
			// The local daemon is restarting (an update): the transfer waits for it and renews then,
			// inside the native recovery window, instead of failing as refused.
			return Auth{}, &localDaemonUnavailable{err}
		}
		if err != nil && machineOffline(err) {
			// The server calls a machine of this transfer offline (its path blipped, its runtime restarts): the
			// transfer waits for it inside the native recovery window and resumes from the committed offset.
			return Auth{}, &machineUnavailable{err}
		}
		if err != nil && serverUnreachable(err) {
			// The renewal could not reach the Paperboat server (the PC just woke from sleep and its network is not back
			// yet): the transfer waits inside the native recovery window and resumes from the committed offset.
			return Auth{}, &serverUnavailable{err}
		}
		if err != nil {
			return Auth{}, &nativeAuthorizationFailure{err}
		}
		if fresh.Lease == nil {
			return Auth{}, &nativeAuthorizationFailure{errors.New("native transfer renewal lacks a lease")}
		}
		if fresh.Endpoint != initial.Endpoint || fresh.Binding != initial.Binding || fresh.Policy != initial.Policy || fresh.Auth.Token == "" || fresh.Auth.ExpiresAt.IsZero() || !fresh.Auth.ExpiresAt.After(serverclock.Now()) {
			return Auth{}, &nativeAuthorizationFailure{errors.Join(errors.New("renewed native transfer grant changed resource or policy"), fresh.Lease.Close())}
		}
		replacement, err := NewNativeRoundTripper(fresh.Lease.OpenTransferStream)
		if err != nil {
			return Auth{}, &nativeAuthorizationFailure{errors.Join(err, fresh.Lease.Close())}
		}
		next := &nativeGrantTransport{grant: fresh, transport: replacement}
		owner.mu.Lock()
		if owner.closed {
			owner.mu.Unlock()
			return Auth{}, errors.Join(net.ErrClosed, closeNativeGrant(next))
		}
		old := owner.current
		if _, exists := owner.grants[fresh.Auth.Token]; exists {
			owner.mu.Unlock()
			return Auth{}, &nativeAuthorizationFailure{errors.Join(errors.New("native transfer renewal reused a credential"), closeNativeGrant(next))}
		}
		old.retired = true
		owner.current = next
		owner.grants[fresh.Auth.Token] = next
		closeOld := old.users == 0
		if closeOld {
			delete(owner.grants, old.grant.Auth.Token)
			owner.cleanup.Add(1)
		}
		owner.mu.Unlock()
		if closeOld {
			owner.retainCleanup(closeNativeGrant(old))
			owner.cleanup.Done()
		}
		return fresh.Auth, nil
	}
	return client, nil
}

type nativeAuthorizationFailure struct{ cause error }

func (e *nativeAuthorizationFailure) Error() string {
	return "file transfer authorization renewal failed"
}
func (e *nativeAuthorizationFailure) Unwrap() error { return e.cause }

func (o *nativeGrantOwner) RoundTrip(request *http.Request) (*http.Response, error) {
	o.mu.Lock()
	grant := o.grants[strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")]
	if o.closed {
		o.mu.Unlock()
		return nil, net.ErrClosed
	}
	if grant == nil {
		o.mu.Unlock()
		return nil, errNativeAuthorizationChanged
	}
	grant.users++
	o.mu.Unlock()
	response, err := grant.transport.RoundTrip(request)
	if err != nil {
		o.release(grant)
		if localDaemonRestarting(err) {
			// The lease's stream goes through the local daemon, which is restarting.
			err = &localDaemonUnavailable{err}
		} else if transferLeaseUnknown(err) {
			err = &transferLeaseLost{err}
		} else if waiting := waitingForPath(err); waiting != nil {
			err = waiting
		}
		return response, err
	}
	if response.Body == nil {
		o.release(grant)
		return response, nil
	}
	response.Body = &nativeGrantBody{ReadCloser: response.Body, release: func() { o.release(grant) }}
	return response, nil
}
func (o *nativeGrantOwner) release(grant *nativeGrantTransport) {
	o.mu.Lock()
	grant.users--
	retire := grant.retired && grant.users == 0 && !o.closed
	if retire {
		delete(o.grants, grant.grant.Auth.Token)
		o.cleanup.Add(1)
	}
	o.mu.Unlock()
	if retire {
		o.retainCleanup(closeNativeGrant(grant))
		o.cleanup.Done()
	}
}
func (o *nativeGrantOwner) retainCleanup(err error) {
	o.mu.Lock()
	o.cleanupErr = errors.Join(o.cleanupErr, err)
	o.mu.Unlock()
}
func (o *nativeGrantOwner) Close() error {
	o.mu.Lock()
	if o.closed {
		err := o.cleanupErr
		o.mu.Unlock()
		return err
	}
	o.closed = true
	grants := o.grants
	o.grants = nil
	o.mu.Unlock()
	var result error
	for _, grant := range grants {
		result = errors.Join(result, closeNativeGrant(grant))
	}
	o.retainCleanup(result)
	o.cleanup.Wait()
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cleanupErr
}
func closeNativeGrant(grant *nativeGrantTransport) error {
	if idle, ok := grant.transport.(interface{ CloseIdleConnections() }); ok {
		idle.CloseIdleConnections()
	}
	return grant.grant.Lease.Close()
}

type nativeGrantBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *nativeGrantBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

// localDaemonUnavailable is a renewal that could not reach the restarting local daemon; it is retried.
type localDaemonUnavailable struct{ cause error }

func (e *localDaemonUnavailable) Error() string {
	return "the local Paperboat daemon is restarting: " + e.cause.Error()
}
func (e *localDaemonUnavailable) Unwrap() error { return e.cause }

// machineUnavailable is a renewal the server refused because a machine of the transfer is offline; it is retried.
type machineUnavailable struct{ cause error }

func (e *machineUnavailable) Error() string {
	return "waiting for the machine to come back online: " + e.cause.Error()
}
func (e *machineUnavailable) Unwrap() error { return e.cause }

// serverUnavailable is a renewal that failed on the network (no route, DNS, timeout) before the server answered; it is retried.
type serverUnavailable struct{ cause error }

func (e *serverUnavailable) Error() string {
	return "waiting for Paperboat to be reachable: " + e.cause.Error()
}
func (e *serverUnavailable) Unwrap() error { return e.cause }

func serverUnreachable(err error) bool {
	var dial *net.OpError
	var lookup *net.DNSError
	var network net.Error
	return errors.As(err, &dial) || errors.As(err, &lookup) || errors.As(err, &network) && network.Timeout()
}

func machineOffline(err error) bool {
	var remote *api.APIError
	return errors.As(err, &remote) && remote.Code == "machine_offline"
}

// transferLeaseLost is a stream the daemon refused because it holds no lease for the handle (it restarted and its leases
// were in memory). Unlike a restarting daemon, retrying the same handle never works: the renewal prepares a new lease.
type transferLeaseLost struct{ cause error }

func (e *transferLeaseLost) Error() string {
	return "the local Paperboat daemon lost the transfer lease: " + e.cause.Error()
}
func (e *transferLeaseLost) Unwrap() error { return e.cause }

func transferLeaseUnknown(err error) bool {
	var local *localapi.RemoteError
	return errors.As(err, &local) && local.StatusCode == http.StatusNotFound && local.Code == localapi.FileTransferUnknownHandleCode
}

// localDaemonRestarting reports whether a renewal or stream failed only because the local daemon is
// restarting: its socket is missing or not accepting yet, or the old daemon, shutting down for an
// update, answered the transfer with 503 before it exited (F14-02).
func localDaemonRestarting(err error) bool {
	var dial *net.OpError
	if errors.As(err, &dial) && dial.Op == "dial" && dial.Net == "unix" &&
		(errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)) {
		return true
	}
	// Windows has no socket handover: the named pipe is gone while the services restart, and its dial error is no
	// unix OpError. localapi wraps every dial failure (not a cancel or a permission refusal) as ErrTransportUnavailable.
	if errors.Is(err, localapi.ErrTransportUnavailable) {
		return true
	}
	var local *localapi.RemoteError
	return errors.As(err, &local) && local.StatusCode == http.StatusServiceUnavailable &&
		(local.Code == "file_transfer_unavailable" || local.Code == "file_transfer_stream_unavailable")
}

// PathWaitingError is an Unlimited transfer waiting for a direct connection or the account's own relay: the
// daemon refused (or closed) its stream because the only path is a Paperboat-hosted relay or none. It is
// retried; the upload resumes from the committed offset.
type PathWaitingError struct {
	Message string
	cause   error
}

func (e *PathWaitingError) Error() string { return e.Message }
func (e *PathWaitingError) Unwrap() error { return e.cause }

func waitingForPath(err error) *PathWaitingError {
	var local *localapi.RemoteError
	if errors.As(err, &local) && local.Code == localapi.NoEligiblePathCode {
		message := local.Message
		if message == "" {
			message = "Waiting for a direct connection or your relay"
		}
		return &PathWaitingError{Message: message, cause: err}
	}
	return nil
}
