package privatepreviewproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type accessConfiguratorStub struct {
	mu          sync.Mutex
	calls       []string
	recoverErr  error
	installErr  error
	removeErr   error
	refreshErr  error
	refreshHook func(context.Context, string) error
}

func (s *accessConfiguratorStub) Recover(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "recover")
	return s.recoverErr
}

func (s *accessConfiguratorStub) Install(_ context.Context, pacURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "install:"+pacURL)
	return s.installErr
}

func (s *accessConfiguratorStub) Refresh(ctx context.Context, pacURL string) error {
	s.mu.Lock()
	s.calls = append(s.calls, "refresh:"+pacURL)
	hook, err := s.refreshHook, s.refreshErr
	s.mu.Unlock()
	if hook != nil {
		return hook(ctx, pacURL)
	}
	return err
}

func (s *accessConfiguratorStub) Remove(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "remove")
	return s.removeErr
}

func (s *accessConfiguratorStub) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func TestAccessServiceOwnsPACAndProxyLifecycle(t *testing.T) {
	configurator := &accessConfiguratorStub{}
	source := &accessTestSource{routes: []AccessRoute{{Hostname: "private.example.test"}}}
	service, err := NewAccessService(AccessServiceConfig{Proxy: AccessProxyConfig{Source: source}, Configurator: configurator, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	pacURL, ok := service.PACURL()
	if !ok || pacURL == "" {
		t.Fatalf("PAC URL = %q, %v", pacURL, ok)
	}
	if got := configurator.snapshot(); len(got) != 2 || got[0] != "recover" || got[1] != "install:"+pacURL {
		t.Fatalf("startup calls = %v", got)
	}
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := configurator.snapshot(); !reflect.DeepEqual(got, []string{"recover", "install:" + pacURL, "remove"}) {
		t.Fatalf("lifecycle calls = %v", got)
	}
	if _, ok := service.PACURL(); ok {
		t.Fatal("PAC remained published after shutdown")
	}
}

func TestAccessServiceFailsClosedBeforePublishingPAC(t *testing.T) {
	recoverFailure := errors.New("recover failed")
	configurator := &accessConfiguratorStub{recoverErr: recoverFailure}
	service, err := NewAccessService(AccessServiceConfig{
		Proxy: AccessProxyConfig{Source: &accessTestSource{}}, Configurator: configurator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); !errors.Is(err, recoverFailure) {
		t.Fatalf("start error = %v", err)
	}
	if got := configurator.snapshot(); !reflect.DeepEqual(got, []string{"recover"}) {
		t.Fatalf("calls = %v", got)
	}
	if _, ok := service.PACURL(); ok {
		t.Fatal("PAC published after failed recovery")
	}
}

func TestAccessServiceClosesListenerWhenPACInstallFails(t *testing.T) {
	installFailure := errors.New("install failed")
	configurator := &accessConfiguratorStub{installErr: installFailure}
	service, err := NewAccessService(AccessServiceConfig{
		Proxy: AccessProxyConfig{Source: &accessTestSource{}}, Configurator: configurator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); !errors.Is(err, installFailure) {
		t.Fatalf("start error = %v", err)
	}
	if _, ok := service.PACURL(); ok {
		t.Fatal("PAC published after failed install")
	}
	if got := configurator.snapshot(); len(got) != 2 || got[0] != "recover" || got[1] == "" {
		t.Fatalf("calls = %v", got)
	}
}

func TestAccessServiceDiscoversNewHostWithoutRestart(t *testing.T) {
	configurator := &accessConfiguratorStub{}
	source := &accessTestSource{routes: []AccessRoute{{Hostname: "first.example.test"}}}
	service, err := NewAccessService(AccessServiceConfig{Proxy: AccessProxyConfig{Source: source}, Configurator: configurator, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	initial, _ := service.PACURL()
	source.mu.Lock()
	source.routes = append(source.routes, AccessRoute{Hostname: "second.example.test"})
	source.mu.Unlock()
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		current, _ := service.PACURL()
		if current != initial {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("new private host did not automatically revise the installed PAC URL")
}

func waitServiceCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("private access condition did not converge")
}

func readServicePAC(t *testing.T, url string) string {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("PAC status=%d err=%v", response.StatusCode, err)
	}
	return string(body)
}

func TestAccessServiceRefreshFailureRetainsUsablePACAndRetries(t *testing.T) {
	configurator := &accessConfiguratorStub{refreshErr: errors.New("platform temporarily unavailable")}
	source := &accessTestSource{routes: []AccessRoute{{Hostname: "first.example.test"}}}
	service, err := NewAccessService(AccessServiceConfig{Proxy: AccessProxyConfig{Source: source}, Configurator: configurator, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	first, _ := service.PACURL()
	source.mu.Lock()
	source.routes = append(source.routes, AccessRoute{Hostname: "second.example.test"})
	source.mu.Unlock()
	waitServiceCondition(t, func() bool { return len(configurator.snapshot()) >= 3 })
	current, _ := service.PACURL()
	if current != first {
		t.Fatal("failed refresh replaced installed PAC")
	}
	if body := readServicePAC(t, first); !strings.Contains(body, "first.example.test") || strings.Contains(body, "second.example.test") {
		t.Fatal("installed revision changed")
	}
	candidate := strings.TrimPrefix(configurator.snapshot()[2], "refresh:")
	if body := readServicePAC(t, candidate); !strings.Contains(body, "second.example.test") {
		t.Fatal("candidate body unavailable for partial apply")
	}
	configurator.mu.Lock()
	configurator.refreshErr = nil
	configurator.mu.Unlock()
	waitServiceCondition(t, func() bool { current, _ := service.PACURL(); return current == candidate })
	if body := readServicePAC(t, candidate); !strings.Contains(body, "second.example.test") {
		t.Fatal("new host missing from live PAC")
	}
}

func TestAccessServiceUnchangedHostSetDoesNotRefresh(t *testing.T) {
	configurator := &accessConfiguratorStub{}
	source := &accessTestSource{routes: []AccessRoute{{Hostname: "second.example.test"}, {Hostname: "first.example.test"}}}
	service, err := NewAccessService(AccessServiceConfig{Proxy: AccessProxyConfig{Source: source}, Configurator: configurator, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	first, _ := service.PACURL()
	source.mu.Lock()
	source.routes[0], source.routes[1] = source.routes[1], source.routes[0]
	source.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	current, _ := service.PACURL()
	if current != first || len(configurator.snapshot()) != 2 {
		t.Fatalf("unchanged hosts churned settings: %v", configurator.snapshot())
	}
}

func TestAccessServiceShutdownCancelsAndJoinsRefresh(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	configurator := &accessConfiguratorStub{refreshHook: func(ctx context.Context, _ string) error {
		close(entered)
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	}}
	source := &accessTestSource{routes: []AccessRoute{{Hostname: "first.example.test"}}}
	service, err := NewAccessService(AccessServiceConfig{Proxy: AccessProxyConfig{Source: source}, Configurator: configurator, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	source.routes = append(source.routes, AccessRoute{Hostname: "second.example.test"})
	source.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh never entered")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- service.Shutdown(context.Background()) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel and join monitor")
	}
	select {
	case <-exited:
	default:
		t.Fatal("shutdown returned before refresh exited")
	}
	calls := configurator.snapshot()
	if calls[len(calls)-1] != "remove" {
		t.Fatalf("shutdown ordering=%v", calls)
	}
}

func TestPACRevisionDependsOnlyOnNormalizedRoutingTuples(t *testing.T) {
	source := &accessTestSource{routes: []AccessRoute{{Hostname: "second.example.test"}, {Hostname: "first.example.test"}}}
	first, err := StartAccessProxy(context.Background(), AccessProxyConfig{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := StartAccessProxy(context.Background(), AccessProxyConfig{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	initial, err := first.publishPAC(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := second.publishPAC(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimPrefix(initial, "http://"+first.ProxyAddress) != strings.TrimPrefix(other, "http://"+second.ProxyAddress) {
		t.Fatal("listener port affected routing revision")
	}
	source.mu.Lock()
	source.routes = []AccessRoute{{Hostname: "FIRST.EXAMPLE.TEST."}, {Hostname: "second.example.test"}}
	source.mu.Unlock()
	unchanged, err := first.publishPAC(context.Background(), initial)
	if err != nil || unchanged != initial {
		t.Fatalf("equivalent routing tuples changed revision: %q %v", unchanged, err)
	}
	source.mu.Lock()
	source.routes[0].MatchType = AccessMatchManagedExact
	source.mu.Unlock()
	changed, err := first.publishPAC(context.Background(), initial)
	if err != nil || changed == initial {
		t.Fatalf("changed match type did not revise tuple digest: %q %v", changed, err)
	}
}

// This source exercises a real HTTP discovery outage through the service's
// existing AccessSource boundary; no routes are invented during the outage.
type accessHTTPDiscoverySource struct {
	*accessTestSource
	endpoint string
}

func (s *accessHTTPDiscoverySource) Snapshot(ctx context.Context) ([]AccessRoute, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return nil, ErrAccessAuthentication
	case http.StatusForbidden:
		return nil, ErrAccessForbidden
	case http.StatusOK:
		var routes []AccessRoute
		err := json.NewDecoder(response.Body).Decode(&routes)
		return routes, err
	default:
		return nil, ErrAccessTemporarilyUnavailable
	}
}

func TestAccessServiceInitialDiscovery503RecoversWithoutRestart(t *testing.T) {
	var calls atomic.Int64
	var status atomic.Int64
	status.Store(http.StatusServiceUnavailable)
	backend := &accessTestSource{routes: []AccessRoute{{Hostname: "first.example.test"}}}
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		code := int(status.Load())
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		routes, _ := backend.Snapshot(r.Context())
		json.NewEncoder(w).Encode(routes)
	}))
	defer discovery.Close()
	source := &accessHTTPDiscoverySource{accessTestSource: backend, endpoint: discovery.URL}
	configurator := &accessConfiguratorStub{}
	service, err := NewAccessService(AccessServiceConfig{Proxy: AccessProxyConfig{Source: source}, Configurator: configurator, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	waitServiceCondition(t, func() bool { return calls.Load() >= 3 })
	if url, ok := service.PACURL(); ok || url != "" {
		t.Fatal("discovery outage published a PAC")
	}
	if got := configurator.snapshot(); !reflect.DeepEqual(got, []string{"recover"}) {
		t.Fatalf("changed OS proxy before discovery: %v", got)
	}
	// Requests during authentication/authorization failures must not reach Open.
	service.mu.Lock()
	address := service.proxy.ProxyAddress
	service.mu.Unlock()
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		status.Store(int64(code))
		request, _ := http.NewRequest(http.MethodConnect, "http://"+address, nil)
		request.Host = "first.example.test:443"
		response, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != code {
			t.Fatalf("denied CONNECT status=%d want=%d", response.StatusCode, code)
		}
	}
	backend.mu.Lock()
	opened := len(backend.hosts)
	backend.mu.Unlock()
	if opened != 0 {
		t.Fatal("denied discovery forwarded a private request")
	}
	status.Store(http.StatusOK)
	waitServiceCondition(t, func() bool { _, ok := service.PACURL(); return ok })
	first, _ := service.PACURL()
	if !strings.Contains(readServicePAC(t, first), "first.example.test") {
		t.Fatal("recovered PAC omitted discovered host")
	}
	backend.mu.Lock()
	backend.routes = append(backend.routes, AccessRoute{Hostname: "second.example.test"})
	backend.mu.Unlock()
	waitServiceCondition(t, func() bool { current, ok := service.PACURL(); return ok && current != first })
	current, _ := service.PACURL()
	if !strings.Contains(readServicePAC(t, current), "second.example.test") {
		t.Fatal("later host did not refresh recovered PAC")
	}
}

func TestAccessServiceInitialInstallRetryRequiresSuccessfulRecovery(t *testing.T) {
	source := &accessTestSource{snapshot: ErrAccessTemporarilyUnavailable, routes: []AccessRoute{{Hostname: "private.example.test"}}}
	configurator := &accessConfiguratorStub{installErr: errors.New("platform unavailable")}
	service, err := NewAccessService(AccessServiceConfig{Proxy: AccessProxyConfig{Source: source}, Configurator: configurator, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	// Freeze recovery before making discovery valid so a failed installation
	// cannot overwrite any settings while ownership remains unresolved.
	configurator.mu.Lock()
	configurator.recoverErr = errors.New("ownership conflict")
	configurator.mu.Unlock()
	source.mu.Lock()
	source.snapshot = nil
	source.mu.Unlock()
	waitServiceCondition(t, func() bool { return len(configurator.snapshot()) >= 4 })
	calls := configurator.snapshot()
	installs := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "install:") {
			installs++
		}
	}
	if installs != 1 {
		t.Fatalf("retried mutation before recovery: %v", calls)
	}
	if _, ok := service.PACURL(); ok {
		t.Fatal("failed install appeared published")
	}
	configurator.mu.Lock()
	configurator.recoverErr = nil
	configurator.installErr = nil
	configurator.mu.Unlock()
	waitServiceCondition(t, func() bool { _, ok := service.PACURL(); return ok })
	current, _ := service.PACURL()
	if !strings.Contains(readServicePAC(t, current), "private.example.test") {
		t.Fatal("recovery did not produce a usable PAC")
	}
}

type accessWaitingDiscoverySource struct {
	*accessTestSource
	calls   atomic.Int64
	entered chan struct{}
}

func (s *accessWaitingDiscoverySource) Snapshot(ctx context.Context) ([]AccessRoute, error) {
	if s.calls.Add(1) == 1 {
		return nil, ErrAccessTemporarilyUnavailable
	}
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAccessServiceShutdownJoinsUnpublishedDiscovery(t *testing.T) {
	source := &accessWaitingDiscoverySource{accessTestSource: &accessTestSource{}, entered: make(chan struct{})}
	configurator := &accessConfiguratorStub{}
	service, err := NewAccessService(AccessServiceConfig{Proxy: AccessProxyConfig{Source: source}, Configurator: configurator, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("discovery retry never entered")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- service.Shutdown(context.Background()) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join unpublished discovery")
	}
	if got := configurator.snapshot(); !reflect.DeepEqual(got, []string{"recover", "remove"}) {
		t.Fatalf("unpublished lifecycle mutated PAC: %v", got)
	}
}
