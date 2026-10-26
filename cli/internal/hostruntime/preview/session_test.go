package preview

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sessionReaderFunc func([]byte) (int, error)

func (f sessionReaderFunc) Read(value []byte) (int, error) { return f(value) }

type sessionTemporaryError struct{}

func (sessionTemporaryError) Error() string   { return "temporary lease transport failure" }
func (sessionTemporaryError) Timeout() bool   { return true }
func (sessionTemporaryError) Temporary() bool { return true }

var _ net.Error = sessionTemporaryError{}

type sessionLeaseClient struct {
	mu         sync.Mutex
	created    []LeaseRequest
	renewed    []Lease
	renewKeys  []string
	stopLeases []Lease
	stopKeys   []string
	renewErr   error
	stopErr    error
	stopDone   chan struct{}
}

type sessionRenewObservationClient struct {
	*sessionLeaseClient
	called chan Lease
}

func (c *sessionRenewObservationClient) Renew(ctx context.Context, lease Lease, idempotencyKey string) (Lease, error) {
	c.called <- lease
	return c.sessionLeaseClient.Renew(ctx, lease, idempotencyKey)
}

func (c *sessionLeaseClient) Create(_ context.Context, request LeaseRequest) (Lease, error) {
	c.mu.Lock()
	c.created = append(c.created, request)
	c.mu.Unlock()
	return sessionTestLease(request), nil
}

func (c *sessionLeaseClient) Renew(_ context.Context, lease Lease, idempotencyKey string) (Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.renewKeys = append(c.renewKeys, idempotencyKey)
	if c.renewErr != nil {
		return Lease{}, c.renewErr
	}
	renewed := lease
	generation := lease.Generation
	if generation < 1 {
		generation = leaseGenerationForID(lease.ID, lease.ETag)
	}
	renewed.ETag = formatLeaseETag(lease.ID, generation+1)
	renewed.LastRenewedAt = time.Now().UTC()
	c.renewed = append(c.renewed, renewed)
	return renewed, nil
}

func (c *sessionLeaseClient) Stop(_ context.Context, lease Lease, idempotencyKey string) error {
	c.mu.Lock()
	c.stopLeases = append(c.stopLeases, lease)
	c.stopKeys = append(c.stopKeys, idempotencyKey)
	if c.stopDone != nil {
		select {
		case <-c.stopDone:
		default:
			close(c.stopDone)
		}
	}
	err := c.stopErr
	c.mu.Unlock()
	return err
}

type sessionCarrier struct {
	mu       sync.Mutex
	runs     int
	runLease []Lease
	closed   chan struct{}
	run      func(context.Context, Lease, func(Lease) error) error
}

func (c *sessionCarrier) Run(ctx context.Context, lease Lease, ready func(Lease) error) error {
	c.mu.Lock()
	c.runs++
	c.runLease = append(c.runLease, lease)
	run := c.run
	c.mu.Unlock()
	if run == nil {
		// A test that never gives the carrier a run still starts the session's carrier goroutine.
		<-ctx.Done()
		return ctx.Err()
	}
	return run(ctx, lease, ready)
}

func (c *sessionCarrier) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed != nil {
		select {
		case <-c.closed:
		default:
			close(c.closed)
		}
	}
	return nil
}

func sessionTestLease(request LeaseRequest) Lease {
	now := time.Now().UTC()
	deadline := now.Add(time.Hour)
	var userDeadline *time.Time
	if request.UserDeadline != nil {
		deadline = request.UserDeadline.UTC()
		userDeadline = &deadline
	}
	return Lease{
		Schema: PreviewTunnelSchemaV1, Kind: PreviewLeaseKind, ID: "prv_session_1", AccountID: "acct_1", ActorID: "actor_1",
		OwnerMachineID: request.OwnerMachineID, OwnerSessionID: request.OwnerSessionID, OwnerSessionKind: request.OwnerSessionKind, Target: request.Target,
		AccessMode: request.AccessMode, Endpoint: "https://quiet-river-7.preview.example.test", EndpointState: "ready", ETag: formatLeaseETag("prv_session_1", 1),
		LeaseDeadline: deadline, UserDeadline: userDeadline, State: "connecting", AllocationState: "pending", EdgeState: "pending", OriginState: "unknown",
		CreatedAt: now, LastRenewedAt: now,
	}
}

func sessionReadyLease(lease Lease) Lease {
	lease.State = "ready"
	lease.AllocationState = "ready"
	lease.EdgeState = "ready"
	lease.OriginState = "ready"
	return lease
}

func newSessionTest(t *testing.T, carrierRun func(context.Context, Lease, func(Lease) error) error) (*Session, *sessionLeaseClient, *sessionCarrier) {
	t.Helper()
	client := &sessionLeaseClient{}
	carrier := &sessionCarrier{closed: make(chan struct{}), run: carrierRun}
	session, err := Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target:        LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"},
		RenewInterval: time.Hour, ShutdownTimeout: time.Second, ReconnectBackoff: time.Millisecond,
		MaxReconnectBackoff: 2 * time.Millisecond, DisableParentWatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session, client, carrier
}

func TestSessionPublishesOnlyAfterFullReadiness(t *testing.T) {
	started := make(chan struct{})
	allowReady := make(chan struct{})
	session, client, carrier := newSessionTest(t, func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		close(started)
		<-allowReady
		if err := ready(sessionReadyLease(lease)); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	defer session.Stop(context.Background())
	<-started
	if url, ok := session.URL(); ok || url != "" {
		t.Fatalf("URL before readiness = %q, %v", url, ok)
	}
	close(allowReady)
	ready, err := session.WaitReady(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ready.Endpoint != "https://quiet-river-7.preview.example.test" {
		t.Fatalf("endpoint = %q", ready.Endpoint)
	}
	if url, ok := session.URL(); !ok || url != ready.Endpoint {
		t.Fatalf("URL after readiness = %q, %v", url, ok)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.created) != 1 || client.created[0].AccessMode != "public" {
		t.Fatalf("create requests = %#v", client.created)
	}
	if len(carrier.runLease) != 1 || carrier.runLease[0].Endpoint != ready.Endpoint {
		t.Fatalf("carrier leases = %#v", carrier.runLease)
	}
}

func TestSessionWaitPreservesOperationalFailureJoinedWithCancellation(t *testing.T) {
	session, _, _ := newSessionTest(t, func(context.Context, Lease, func(Lease) error) error {
		return errors.Join(context.Canceled, io.ErrUnexpectedEOF)
	})

	err := session.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error lost cancellation: %v", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("wait error lost original carrier failure: %v", err)
	}
}

func TestSessionWaitPreservesParentCancellationCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	started := make(chan struct{})
	client := &sessionLeaseClient{}
	carrier := &sessionCarrier{closed: make(chan struct{}), run: func(ctx context.Context, _ Lease, _ func(Lease) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	session, err := Start(ctx, SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target:        LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"},
		RenewInterval: time.Hour, ShutdownTimeout: time.Second, DisableParentWatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel(io.ErrUnexpectedEOF)
	err = session.Wait()
	if !errors.Is(err, context.Canceled) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("wait error did not preserve cancellation and parent cause: %v", err)
	}
}

func TestSessionEndpointProjectionCanBecomeReadyAndStaleWithoutChangingIdentity(t *testing.T) {
	session, _, _ := newSessionTest(t, func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		if err := ready(sessionReadyLease(lease)); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := session.Stop(ctx); err != nil {
			t.Errorf("stop session: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	initial := session.currentLease()
	custom := initial
	custom.Endpoint = "https://app.customer.example"
	custom.EndpointState = "ready"
	custom.Generation = 2
	custom.ETag = formatLeaseETag(custom.ID, custom.Generation)
	custom.LastRenewedAt = time.Now().UTC()
	if err := session.acceptRenewal(custom, initial); err != nil {
		t.Fatalf("accept customer endpoint: %v", err)
	}
	if endpoint, ok := session.URL(); !ok || endpoint != custom.Endpoint {
		t.Fatalf("URL after custom domain became ready = %q, ready=%v", endpoint, ok)
	}

	stale := custom
	stale.Endpoint = ""
	stale.EndpointState = "custom_domain_not_ready"
	stale.State = "connecting"
	stale.AllocationState = "pending"
	stale.Generation = 3
	stale.ETag = formatLeaseETag(stale.ID, stale.Generation)
	stale.LastRenewedAt = time.Now().UTC()
	if err := session.acceptRenewal(stale, custom); err != nil {
		t.Fatalf("accept stale custom endpoint: %v", err)
	}
	if endpoint, ok := session.URL(); ok || endpoint != "" {
		t.Fatalf("URL after domain became stale = %q, ready=%v; want no URL", endpoint, ok)
	}

	recovered := sessionReadyLease(stale)
	recovered.Endpoint = "https://app.customer.example"
	recovered.EndpointState = "ready"
	recovered.Generation = 4
	recovered.ETag = formatLeaseETag(recovered.ID, recovered.Generation)
	recovered.LastRenewedAt = time.Now().UTC()
	if err := session.markReady(recovered); err != nil {
		t.Fatalf("accept recovered endpoint: %v", err)
	}
	if endpoint, ok := session.URL(); !ok || endpoint != recovered.Endpoint {
		t.Fatalf("URL after domain recovered = %q, ready=%v", endpoint, ok)
	}
}

func TestSessionRetriesCarrierReconnectWithoutChangingURL(t *testing.T) {
	var attempts int
	first := make(chan struct{})
	session, client, carrier := newSessionTest(t, func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		attempts++
		if attempts == 1 {
			close(first)
			return &RetryableCarrierError{Err: errors.New("edge disconnected")}
		}
		if err := ready(sessionReadyLease(lease)); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	<-first
	ready, err := session.WaitReady(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := session.URL(); !ok || got != ready.Endpoint {
		t.Fatalf("reconnected URL = %q, %v", got, ok)
	}
	if err := session.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	carrier.mu.Lock()
	if carrier.runs != 2 || carrier.runLease[0].ID != carrier.runLease[1].ID || carrier.runLease[0].Endpoint != carrier.runLease[1].Endpoint {
		t.Fatalf("carrier reconnect leases = %#v", carrier.runLease)
	}
	carrier.mu.Unlock()
	client.mu.Lock()
	if len(client.stopLeases) != 1 {
		t.Fatalf("stop calls = %d", len(client.stopLeases))
	}
	client.mu.Unlock()
}

func TestSessionOwnerLossStopsCarrierAndRevokesLease(t *testing.T) {
	ownerDone := make(chan struct{})
	client := &sessionLeaseClient{}
	carrier := &sessionCarrier{closed: make(chan struct{}), run: func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		if err := ready(sessionReadyLease(lease)); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	session, err := Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, OwnerDone: ownerDone,
		RenewInterval: time.Hour, ShutdownTimeout: time.Second, DisableParentWatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(ownerDone)
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	carrier.mu.Lock()
	select {
	case <-carrier.closed:
	default:
		t.Fatal("carrier was not closed")
	}
	carrier.mu.Unlock()
	client.mu.Lock()
	if len(client.stopLeases) != 1 || client.stopLeases[0].ID != "prv_session_1" {
		t.Fatalf("stop calls = %#v", client.stopLeases)
	}
	client.mu.Unlock()
}

func TestSessionRenewalLossStopsTheCarrier(t *testing.T) {
	client := &sessionLeaseClient{renewErr: errors.New("session expired")}
	carrier := &sessionCarrier{closed: make(chan struct{}), run: func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		if err := ready(sessionReadyLease(lease)); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	session, err := Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target:        LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"},
		RenewInterval: time.Millisecond, ShutdownTimeout: time.Second, DisableParentWatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Renewal fails every millisecond, so the loss can land before the ready lease is observed.
	if _, err := session.WaitReady(context.Background()); err != nil && !errors.Is(err, ErrLeaseLost) {
		t.Fatal(err)
	}
	err = session.Wait()
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wait error = %v", err)
	}
	carrier.mu.Lock()
	select {
	case <-carrier.closed:
	default:
		t.Fatal("carrier was not closed after lease loss")
	}
	carrier.mu.Unlock()
}

func TestSessionDurationBecomesMaximumUserDeadline(t *testing.T) {
	client := &sessionLeaseClient{}
	carrier := &sessionCarrier{closed: make(chan struct{}), run: func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	session, err := Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, Duration: time.Minute,
		RenewInterval: time.Hour, ShutdownTimeout: time.Second, DisableParentWatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	if len(client.created) != 1 || client.created[0].UserDeadline == nil || time.Until(*client.created[0].UserDeadline) <= 0 {
		t.Fatalf("create deadline = %#v", client.created)
	}
	client.mu.Unlock()
	if err := session.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionExpiryUsesInjectedClock(t *testing.T) {
	base := time.Now().UTC()
	var calls atomic.Int32
	now := func() time.Time {
		if calls.Add(1) == 1 {
			return base
		}
		return base.Add(2 * time.Hour)
	}
	client := &sessionLeaseClient{}
	carrier := &sessionCarrier{closed: make(chan struct{}), run: func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	session, err := Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, Now: now,
		RenewInterval: time.Hour, ShutdownTimeout: time.Second, DisableParentWatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("injected-clock expiry error = %v", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("injected clock was not consulted by renew loop: %d calls", calls.Load())
	}
}

func TestSessionIdempotencyKeyUsesInjectedRandomSource(t *testing.T) {
	var next byte
	reader := sessionReaderFunc(func(value []byte) (int, error) {
		for index := range value {
			value[index] = next
			next++
		}
		return len(value), nil
	})
	first, err := newSessionIdempotencyKey(reader)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newSessionIdempotencyKey(reader)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(first) < len("preview_")+1 || len(second) < len("preview_")+1 {
		t.Fatalf("keys are not unique: %q %q", first, second)
	}

	client := &sessionLeaseClient{}
	carrier := &sessionCarrier{run: func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	_, err = Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, Random: sessionReaderFunc(func([]byte) (int, error) {
			return 0, io.ErrUnexpectedEOF
		}),
	})
	if !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("random source failure = %v", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("random source failure lost its cause: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.created) != 0 {
		t.Fatalf("create called after key failure: %d", len(client.created))
	}

	client = &sessionLeaseClient{}
	_, err = Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, IdempotencyKey: "same-key", StopIdempotencyKey: "same-key",
	})
	if !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("duplicate mutation keys error = %v", err)
	}
}

func TestSessionParentDeathStopsAndRevokes(t *testing.T) {
	var parent atomic.Int32
	parent.Store(100)
	client := &sessionLeaseClient{}
	carrier := &sessionCarrier{closed: make(chan struct{}), run: func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		if err := ready(sessionReadyLease(lease)); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	session, err := Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, ParentPID: func() int { return int(parent.Load()) },
		ParentPollInterval: time.Millisecond, RenewInterval: time.Hour, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	parent.Store(101)
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	if len(client.stopLeases) != 1 {
		t.Fatalf("stop calls = %d", len(client.stopLeases))
	}
	client.mu.Unlock()
}

func TestSessionExpiryRevokesOnceAfterRenewalRetriesReachDeadline(t *testing.T) {
	client := &sessionLeaseClient{renewErr: sessionTemporaryError{}}
	carrier := &sessionCarrier{closed: make(chan struct{}), run: func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		if err := ready(sessionReadyLease(lease)); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	// Several renewals fit before the deadline even when the host is slow to schedule the first one.
	deadline := time.Now().UTC().Add(600 * time.Millisecond)
	session, err := Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, UserDeadline: &deadline,
		RenewInterval: 30 * time.Millisecond, ReconnectBackoff: 10 * time.Millisecond, MaxReconnectBackoff: 10 * time.Millisecond,
		ShutdownTimeout: time.Second, DisableParentWatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = session.Wait()
	if !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expiry wait error = %v", err)
	}
	client.mu.Lock()
	if len(client.stopLeases) != 1 {
		t.Fatalf("stop calls = %d", len(client.stopLeases))
	}
	if len(client.renewKeys) < 2 || client.renewKeys[0] == "" {
		t.Fatalf("renew retry keys = %#v", client.renewKeys)
	}
	for _, key := range client.renewKeys[1:] {
		if key != client.renewKeys[0] {
			t.Fatalf("renew retry changed idempotency key: %#v", client.renewKeys)
		}
	}
	if client.stopKeys[0] == "" {
		t.Fatalf("stop idempotency key is empty: %#v", client.stopKeys)
	}
	client.mu.Unlock()
	if err := session.Stop(context.Background()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("idempotent stop error = %v", err)
	}
	client.mu.Lock()
	if len(client.stopKeys) != 1 || client.stopKeys[0] == client.renewKeys[0] {
		t.Fatalf("stop key reuse = %#v renew=%#v", client.stopKeys, client.renewKeys)
	}
	client.mu.Unlock()
}

func TestSessionRenewalUsesLeaseAdvancedDuringRenewalWait(t *testing.T) {
	base := time.Now().UTC()
	captured := make(chan struct{})
	release := make(chan struct{})
	var nowCalls atomic.Int32
	now := func() time.Time {
		if nowCalls.Add(1) == 2 {
			close(captured)
			<-release
		}
		return base
	}

	baseClient := &sessionLeaseClient{}
	renewCalled := make(chan Lease, 1)
	client := &sessionRenewObservationClient{sessionLeaseClient: baseClient, called: renewCalled}
	carrier := &sessionCarrier{closed: make(chan struct{}), run: func(ctx context.Context, _ Lease, _ func(Lease) error) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	session, err := Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"},
		Now:    now, RenewInterval: time.Millisecond, ShutdownTimeout: time.Second, DisableParentWatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop(context.Background())

	<-captured
	session.mu.Lock()
	session.lease.CreateOperationID = "operation_renew_ready"
	session.mu.Unlock()
	ready := session.currentLease()
	ready = sessionReadyLease(ready)
	ready.ETag = formatLeaseETag(ready.ID, 2)
	ready.CreateOperationID = "" // readiness projections omit the create operation
	if err := session.markReady(ready); err != nil {
		t.Fatal(err)
	}
	if got := session.currentLease().CreateOperationID; got != "operation_renew_ready" {
		t.Fatalf("readiness lost create operation: %q", got)
	}
	close(release)

	select {
	case renewedWith := <-renewCalled:
		if renewedWith.Generation != 2 || renewedWith.ETag != formatLeaseETag(ready.ID, 2) {
			t.Fatalf("renew used stale lease = %+v", renewedWith)
		}
	case <-time.After(time.Second):
		t.Fatal("renewal did not start")
	}
	if _, err := session.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestObservedSessionDelegatesRenewAndStopToStableRuntime(t *testing.T) {
	client := &sessionLeaseClient{}
	carrier := &sessionCarrier{closed: make(chan struct{}), run: func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		lease = sessionReadyLease(lease)
		lease.ETag = formatLeaseETag(lease.ID, 2)
		if err := ready(lease); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	session, err := Start(context.Background(), SessionConfig{
		LeaseClient: client, Carrier: carrier, OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, LeaseLifecycle: LeaseLifecycleObserved,
		RenewInterval: time.Millisecond, ShutdownTimeout: time.Second, DisableParentWatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := session.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.renewed) != 0 || len(client.stopLeases) != 0 {
		t.Fatalf("observer mutated server lease: renew=%d stop=%d", len(client.renewed), len(client.stopLeases))
	}
}

// A terminal lease says endpoint_state "ended" with no endpoint: the daemon accepts it as a valid lease and a
// renewal that returns it ends the session (ErrLeaseLost); "ended" with a live state is refused.
func TestSessionAcceptsEndedLeaseAsTerminal(t *testing.T) {
	session, _, _ := newSessionTest(t, nil)
	defer session.Stop(context.Background())
	session.mu.Lock()
	current := session.lease
	session.mu.Unlock()
	ended := current
	ended.Endpoint, ended.EndpointState, ended.State = "", "ended", "stopped"
	if err := validateSessionLease(ended, session.config, time.Now()); err != nil {
		t.Fatalf("ended stopped lease refused: %v", err)
	}
	ended.Generation = current.Generation + 1
	ended.ETag = formatLeaseETag(ended.ID, ended.Generation)
	if err := session.acceptRenewal(ended, current); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renewal to an ended lease: %v, want ErrLeaseLost", err)
	}
	ended.State = "ready"
	if err := validateSessionLease(ended, session.config, time.Now()); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("ended ready lease accepted: %v", err)
	}
}
