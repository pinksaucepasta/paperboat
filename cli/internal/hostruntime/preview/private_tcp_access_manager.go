package preview

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
	"github.com/pinksaucepasta/paperboat/shared/admission"
)

const (
	PrivateTCPAccessSchema       = "paperboat.private-tcp-access/v1"
	privateTCPAccessBodyLimit    = 4 << 10
	defaultPrivateTCPAccessLimit = 64
)

var (
	ErrPrivateTCPAccessInvalid        = errors.New("invalid private TCP access request")
	ErrPrivateTCPAccessAuthentication = errors.New("private TCP access authentication required")
	ErrPrivateTCPAccessForbidden      = errors.New("private TCP access forbidden")
	ErrPrivateTCPAccessNotFound       = errors.New("private TCP route not found")
	ErrPrivateTCPAccessUnavailable    = errors.New("private TCP access temporarily unavailable")
	ErrPrivateTCPAccessClosed         = errors.New("private TCP access manager closed")
)

type PrivateTCPAccessManagerConfig struct {
	Runtime       *MachinePreviewRuntime
	ControlToken  string
	RunContext    context.Context
	MaximumActive int
	resolve       func(context.Context, string) (string, string, error)
	start         func(context.Context, PrivateTCPAccessRequest) (privateTCPAccessProxy, error)
	validate      func(context.Context, string, string) (time.Time, error)
	newID         func() (string, error)
}

type privateTCPAccessProxy interface {
	Close() error
	AccessURL() string
}
type machinePrivateTCPProxy struct{ proxy *privatepreviewproxy.Proxy }

func (p machinePrivateTCPProxy) Close() error      { return p.proxy.Close() }
func (p machinePrivateTCPProxy) AccessURL() string { return p.proxy.URL }

type PrivateTCPAccessManager struct {
	token      string
	runContext context.Context
	maximum    int
	resolve    func(context.Context, string) (string, string, error)
	start      func(context.Context, PrivateTCPAccessRequest) (privateTCPAccessProxy, error)
	validate   func(context.Context, string, string) (time.Time, error)
	newID      func() (string, error)
	lifetime   *admission.Lease
	mu         sync.Mutex
	closed     bool
	closeDone  chan struct{}
	closeErr   error
	pending    int
	active     map[string]*privateTCPAccessSession
}

type privateTCPAccessSession struct {
	ID, TunnelID, RouteID, ListenAddress string
	proxy                                privateTCPAccessProxy
	lifetime                             *admission.Lease
	closeOnce                            sync.Once
	onClosed                             func()
	closeErr                             error
}
type privateTCPAccessRequestDocument struct {
	Schema        string `json:"schema"`
	Kind          string `json:"kind"`
	Selector      string `json:"selector"`
	ListenAddress string `json:"listen_address"`
}
type privateTCPAccessResponseDocument struct {
	Schema        string `json:"schema"`
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	TunnelID      string `json:"tunnel_id"`
	RouteID       string `json:"route_id"`
	ListenAddress string `json:"listen_address"`
}

func NewPrivateTCPAccessManager(config PrivateTCPAccessManagerConfig) (*PrivateTCPAccessManager, error) {
	if config.RunContext == nil || strings.TrimSpace(config.ControlToken) == "" {
		return nil, ErrPrivateTCPAccessInvalid
	}
	if config.MaximumActive == 0 {
		config.MaximumActive = defaultPrivateTCPAccessLimit
	}
	if config.MaximumActive < 1 || config.MaximumActive > 1024 {
		return nil, ErrPrivateTCPAccessInvalid
	}
	resolve, start := config.resolve, config.start
	validate := config.validate

	if config.Runtime != nil {
		if validate == nil {
			validate = config.Runtime.validatePrivateTCPRoute
		}
		if resolve == nil {
			resolve = config.Runtime.resolvePrivateTCPRoute
		}
		if start == nil {
			start = func(ctx context.Context, request PrivateTCPAccessRequest) (privateTCPAccessProxy, error) {
				proxy, err := config.Runtime.StartPrivateTCPAccess(ctx, request)
				if err != nil {
					return nil, err
				}
				return machinePrivateTCPProxy{proxy}, nil
			}
		}
	}
	if resolve == nil || start == nil {
		return nil, ErrPrivateTCPAccessInvalid
	}
	newID := config.newID
	if newID == nil {
		newID = newPrivateAccessIdentifier
	}
	manager := &PrivateTCPAccessManager{token: strings.TrimSpace(config.ControlToken), runContext: config.RunContext, maximum: config.MaximumActive, resolve: resolve, start: start, validate: validate, newID: newID, active: make(map[string]*privateTCPAccessSession), closeDone: make(chan struct{})}
	owner, err := admission.Admit(config.RunContext, time.Time{}, nil)
	if err != nil {
		return nil, err
	}
	manager.lifetime = owner
	manager.runContext = owner.Context()
	go func() { <-owner.Context().Done(); _ = manager.Close() }()
	return manager, nil
}

func (r *MachinePreviewRuntime) lookupPrivateTCPAdmission(ctx context.Context, selector string) (accessorAdmission, error) {
	if r == nil || ctx == nil || !validPrivateTCPSelector(selector) {
		return accessorAdmission{}, ErrPrivateTCPAccessInvalid
	}
	selector = strings.TrimSpace(selector)
	r.mu.Lock()
	if r.closed || r.private == nil {
		r.mu.Unlock()
		return accessorAdmission{}, ErrPrivateTCPAccessUnavailable
	}
	source := r.private
	r.mu.Unlock()
	source.mu.RLock()
	discovery := source.discovery
	closed := source.closed
	source.mu.RUnlock()
	if closed || discovery == nil {
		return accessorAdmission{}, ErrPrivateTCPAccessUnavailable
	}
	admissions, err := discovery.snapshot(ctx)
	if err != nil {
		return accessorAdmission{}, mapPrivateTCPAccessError(err)
	}
	selected, err := resolvePrivateTCPAdmission(admissions, selector)
	if err != nil {
		return accessorAdmission{}, err
	}
	return selected, nil
}

func (r *MachinePreviewRuntime) resolvePrivateTCPRoute(ctx context.Context, selector string) (string, string, error) {
	selected, err := r.lookupPrivateTCPAdmission(ctx, selector)
	if err != nil {
		return "", "", err
	}
	return selected.RouteID, selected.TunnelID, nil
}

func (r *MachinePreviewRuntime) validatePrivateTCPRoute(ctx context.Context, tunnelID, routeID string) (time.Time, error) {
	selected, err := r.lookupPrivateTCPAdmission(ctx, routeID)
	if err != nil {
		return time.Time{}, err
	}
	if selected.TunnelID != tunnelID {
		return time.Time{}, ErrPrivateTCPAccessForbidden
	}
	return selected.ExpiresAt, nil
}

func resolvePrivateTCPAdmission(admissions []accessorAdmission, selector string) (accessorAdmission, error) {
	if !validPrivateTCPSelector(selector) {
		return accessorAdmission{}, ErrPrivateTCPAccessInvalid
	}
	selector = strings.TrimSpace(selector)
	var selected *accessorAdmission
	for i := range admissions {
		a := &admissions[i]
		if a.ResourceKind != "tunnel" || a.Protocol != "private_tcp" {
			continue
		}
		if !matchesPrivateTCPSelector(*a, selector) {
			continue
		}
		if selected != nil && selected.RouteID != a.RouteID {
			return accessorAdmission{}, ErrPrivateTCPAccessUnavailable
		}
		if selected == nil {
			selected = a // the same route on the other edge of the pair is the same answer
		}
	}
	if selected == nil {
		return accessorAdmission{}, ErrPrivateTCPAccessNotFound
	}
	return *selected, nil
}

func matchesPrivateTCPSelector(admission accessorAdmission, selector string) bool {
	return admission.RouteID == selector || admission.ResourceID == selector || admission.TunnelID == selector || admission.TunnelName == selector || admission.RouteName == selector
}

func (m *PrivateTCPAccessManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if m == nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))), []byte(m.token)) != 1 {
		writePrivateTCPAccessError(w, http.StatusUnauthorized, "authentication_required", "Authentication with the local hostd control token is required.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/private-tcp-access")
	switch {
	case r.Method == http.MethodPost && path == "":
		m.handleStart(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/") && len(path) > 1:
		m.handleDelete(w, strings.TrimPrefix(path, "/"))
	default:
		writePrivateTCPAccessError(w, http.StatusNotFound, "not_found", "Private TCP access endpoint was not found.")
	}
}

func (m *PrivateTCPAccessManager) handleStart(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, privateTCPAccessBodyLimit+1))
	if err != nil || len(body) == 0 || len(body) > privateTCPAccessBodyLimit || rejectAttachmentDuplicateFields(body) != nil {
		writePrivateTCPAccessError(w, http.StatusBadRequest, "invalid_request", "The private TCP access request is invalid.")
		return
	}
	var document privateTCPAccessRequestDocument
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || decoder.Decode(&struct{}{}) != io.EOF || document.Schema != PrivateTCPAccessSchema || document.Kind != "private_tcp_access_request" || !validPrivateTCPSelector(document.Selector) {
		writePrivateTCPAccessError(w, http.StatusBadRequest, "invalid_request", "The private TCP access request is invalid.")
		return
	}
	listenPort, err := privateTCPListenPort(document.ListenAddress)
	if err != nil {
		writePrivateTCPAccessError(w, http.StatusBadRequest, "invalid_listen_address", "The listen address must be literal loopback.")
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		writePrivateTCPAccessError(w, http.StatusServiceUnavailable, "runtime_unavailable", "Stable hostd is shutting down.")
		return
	}
	if len(m.active)+m.pending >= m.maximum {
		m.mu.Unlock()
		writePrivateTCPAccessError(w, http.StatusServiceUnavailable, "capacity_exhausted", "The private TCP access limit is reached.")
		return
	}
	m.pending++
	m.mu.Unlock()
	reserved := true
	defer func() {
		if reserved {
			m.mu.Lock()
			m.pending--
			m.mu.Unlock()
		}
	}()
	routeID, tunnelID, err := m.resolve(r.Context(), document.Selector)
	if err != nil {
		logPrivateTCPAccessFailure(r.Context(), "resolve", err)
		writeMappedPrivateTCPAccessError(w, err)
		return
	}
	var verifiedUntil time.Time
	if m.validate != nil {
		verifiedUntil, err = m.validate(r.Context(), tunnelID, routeID)
		if err != nil || !verifiedUntil.After(time.Now().UTC()) {
			logPrivateTCPAccessFailure(r.Context(), "validate", err)
			writeMappedPrivateTCPAccessError(w, mapPrivateTCPAccessError(err))
			return
		}
	}
	lease, err := admission.Admit(m.runContext, verifiedUntil, nil)
	if err != nil {
		logPrivateTCPAccessFailure(r.Context(), "admit", err)
		writeMappedPrivateTCPAccessError(w, mapPrivateTCPAccessError(err))
		return
	}
	retained := false
	defer func() {
		if !retained {
			lease.Close()
		}
	}()
	// The listener is owned by stable hostd, not by the request that created it.
	// The post-start closed check below cleans up a concurrent shutdown.
	proxy, err := m.start(lease.Context(), PrivateTCPAccessRequest{RouteID: routeID, ListenPort: listenPort, ListenAddress: strings.TrimSpace(document.ListenAddress), MaximumConnections: 128})
	if err != nil {
		logPrivateTCPAccessFailure(r.Context(), "start", err)
		writeMappedPrivateTCPAccessError(w, mapPrivateTCPAccessError(err))
		return
	}
	listenAddress, err := privateTCPProxyAddress(proxy.AccessURL())
	if err != nil {
		_ = proxy.Close()
		writePrivateTCPAccessError(w, http.StatusServiceUnavailable, "runtime_unavailable", "The authorized loopback listener could not be started.")
		return
	}
	id, err := m.newID()
	if err != nil || !validPrivateTCPSelector(id) {
		_ = proxy.Close()
		writePrivateTCPAccessError(w, http.StatusServiceUnavailable, "runtime_unavailable", "The access session could not be created.")
		return
	}
	session := &privateTCPAccessSession{ID: "access_" + id, TunnelID: tunnelID, RouteID: routeID, ListenAddress: listenAddress, proxy: proxy, lifetime: lease}
	m.mu.Lock()
	m.pending--
	reserved = false
	if _, exists := m.active[session.ID]; m.closed || len(m.active) >= m.maximum || exists {
		m.mu.Unlock()
		_ = proxy.Close()
		writePrivateTCPAccessError(w, http.StatusServiceUnavailable, "capacity_exhausted", "The private TCP access limit is reached.")
		return
	}
	session.onClosed = func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.active[session.ID] == session {
			delete(m.active, session.ID)
		}
	}
	m.active[session.ID] = session
	retained = true
	lease.Bind(session)
	m.mu.Unlock()
	if m.validate != nil {
		lease.Poll(func(check context.Context) error {
			_, err := m.validate(check, session.TunnelID, session.RouteID)
			if !errors.Is(err, ErrPrivateTCPAccessForbidden) && !errors.Is(err, ErrPrivateTCPAccessNotFound) {
				return err
			}
			return admission.ErrRevoked
		})
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(privateTCPAccessResponseDocument{Schema: PrivateTCPAccessSchema, Kind: "private_tcp_access", ID: session.ID, TunnelID: tunnelID, RouteID: routeID, ListenAddress: listenAddress})
}

func (m *PrivateTCPAccessManager) handleDelete(w http.ResponseWriter, id string) {
	if !validPrivateTCPSelector(id) {
		writePrivateTCPAccessError(w, http.StatusNotFound, "not_found", "Private TCP access session was not found.")
		return
	}
	m.mu.Lock()
	session := m.active[id]
	m.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
	w.Header().Del("Content-Type")
	w.WriteHeader(http.StatusNoContent)
}

func (m *PrivateTCPAccessManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-m.closeDone
		return m.closeErr
	}
	m.closed = true
	sessions := make([]*privateTCPAccessSession, 0, len(m.active))
	for _, session := range m.active {
		sessions = append(sessions, session)
	}
	clear(m.active)
	m.mu.Unlock()
	if m.lifetime != nil {
		m.lifetime.Close()
	}
	var result error
	for _, session := range sessions {
		result = errors.Join(result, session.Close())
	}
	m.closeErr = result
	close(m.closeDone)
	return result
}

func (s *privateTCPAccessSession) Close() error {
	s.closeOnce.Do(func() { s.lifetime.Close(); s.closeErr = s.proxy.Close(); s.onClosed() })
	return s.closeErr
}

func validPrivateTCPSelector(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n/\\")
}
func privateTCPListenPort(value string) (uint16, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return 0, ErrPrivateTCPAccessInvalid
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() || (host != "127.0.0.1" && host != "::1") {
		return 0, ErrPrivateTCPAccessInvalid
	}
	parsed, err := strconv.ParseUint(port, 10, 16)
	if err != nil || strconv.FormatUint(parsed, 10) != port {
		return 0, ErrPrivateTCPAccessInvalid
	}
	return uint16(parsed), nil
}
func privateTCPProxyAddress(raw string) (string, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", ErrPrivateTCPAccessUnavailable
	}
	host, port, err := net.SplitHostPort(endpoint.Host)
	if err != nil || host != "127.0.0.1" && host != "::1" {
		return "", ErrPrivateTCPAccessUnavailable
	}
	parsed, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsed == 0 {
		return "", ErrPrivateTCPAccessUnavailable
	}
	return net.JoinHostPort(host, port), nil
}
func mapPrivateTCPAccessError(err error) error {
	switch {
	case errors.Is(err, privatepreviewproxy.ErrAccessAuthentication):
		return ErrPrivateTCPAccessAuthentication
	case errors.Is(err, privatepreviewproxy.ErrAccessForbidden):
		return ErrPrivateTCPAccessForbidden
	case errors.Is(err, ErrPrivateTCPAccessNotFound):
		return ErrPrivateTCPAccessNotFound
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return ErrPrivateTCPAccessUnavailable
	}
}

// The local caller only sees a mapped code; the cause stays in the hostd log.
func logPrivateTCPAccessFailure(ctx context.Context, step string, err error) {
	errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "private_tcp_access", step, "access_refused", err)
}

func writeMappedPrivateTCPAccessError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrPrivateTCPAccessAuthentication):
		writePrivateTCPAccessError(w, http.StatusUnauthorized, "authentication_required", "Paperboat authentication is required.")
	case errors.Is(err, ErrPrivateTCPAccessForbidden):
		writePrivateTCPAccessError(w, http.StatusForbidden, "access_forbidden", "Private access is not allowed.")
	case errors.Is(err, ErrPrivateTCPAccessNotFound):
		// Do not disclose whether the selector names an absent, paused, or
		// cross-account route. An authenticated caller without a current
		// authorization receives the same non-enumerating denial.
		writePrivateTCPAccessError(w, http.StatusForbidden, "access_forbidden", "Private access is not allowed.")
	default:
		writePrivateTCPAccessError(w, http.StatusServiceUnavailable, "runtime_unavailable", "Private access is temporarily unavailable.")
	}
}
func writePrivateTCPAccessError(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
}

var _ http.Handler = (*PrivateTCPAccessManager)(nil)
