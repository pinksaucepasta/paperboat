package runtime

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
	"net"
	"net/http"
	"sync"
	"time"
)

type AssemblySpec struct {
	Reporter    *reporting.Reporter
	Persistence Component
	Control     Component
	// Carrier owns authenticated connector-v1 carrier listeners and accepted
	// peers. It is optional for deployments which have not supplied the
	// server-owned endpoint/certificate/authorizer material yet.
	Carrier Component
	// Certificates is the optional live server-to-edge certificate
	// distribution worker. It owns only in-memory edge key material.
	Certificates Component
	// Preview reconciles server-issued preview carrier admissions and must stop
	// before Carrier so route detach observations can use live peer handles.
	Runtime               Component
	Preview               Component
	Node                  Component
	Routes                Component
	PublicTCP             Component
	Usage                 Component
	GatewayAddress        string
	GatewayHandler        http.Handler
	GatewayTLS            *tls.Config
	GatewayMaxHeaderBytes int
	GatewayWrapListener   func(net.Listener) (net.Listener, error)
	PrivateAddress        string
	PrivateHandler        http.Handler
	PrivateTLS            *tls.Config
	RedirectAddress       string
	RedirectHandler       http.Handler
}

type Assembly struct {
	dataPlane   *DataPlane
	Gateway     *HTTPServer
	Private     *HTTPServer
	Redirect    *HTTPServer
	HTTP3       *HTTP3Server
	done        chan error
	carrierDone <-chan error
	stop        chan struct{}
	stopOnce    sync.Once
}

func NewAssembly(spec AssemblySpec) (*Assembly, error) {
	if spec.Persistence == nil || spec.Control == nil || spec.Node == nil || spec.Routes == nil || spec.Usage == nil {
		return nil, fmt.Errorf("assembly dependencies: %w", ErrProcessInvalid)
	}
	if spec.GatewayAddress == "" || spec.GatewayHandler == nil {
		return nil, fmt.Errorf("assembly gateway: %w", ErrProcessInvalid)
	}
	gateway, err := NewHTTPServer(HTTPServerSpec{Reporter: spec.Reporter, Address: spec.GatewayAddress, Handler: spec.GatewayHandler, TLSConfig: spec.GatewayTLS, WrapListener: spec.GatewayWrapListener, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: spec.GatewayMaxHeaderBytes, Role: HTTPServerPublicTLS})
	if err != nil {
		return nil, fmt.Errorf("assembly gateway: %w", err)
	}
	private, err := NewHTTPServer(HTTPServerSpec{Reporter: spec.Reporter, Address: spec.PrivateAddress, Handler: spec.PrivateHandler, TLSConfig: spec.PrivateTLS, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: spec.GatewayMaxHeaderBytes})
	if err != nil {
		return nil, fmt.Errorf("assembly private ingress: %w", err)
	}
	redirect, err := NewHTTPServer(HTTPServerSpec{Reporter: spec.Reporter, Address: spec.RedirectAddress, Handler: spec.RedirectHandler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 32 << 10, Role: HTTPServerPublicRedirect})
	if err != nil {
		return nil, fmt.Errorf("assembly redirect ingress: %w", err)
	}
	http3Server, err := NewHTTP3Server(spec.GatewayAddress, spec.GatewayHandler, spec.GatewayTLS, spec.GatewayMaxHeaderBytes)
	if err != nil {
		return nil, fmt.Errorf("assembly HTTP/3 ingress: %w", err)
	}
	http3Server.Reporter = spec.Reporter
	dataPlane, err := NewDataPlane(DataPlaneSpec{
		Persistence:     spec.Persistence,
		Control:         spec.Control,
		Carrier:         spec.Carrier,
		Certificates:    spec.Certificates,
		Preview:         spec.Preview,
		Runtime:         spec.Runtime,
		Node:            spec.Node,
		Routes:          spec.Routes,
		PublicTCP:       spec.PublicTCP,
		Usage:           spec.Usage,
		Gateway:         gateway,
		PrivateIngress:  private,
		RedirectIngress: redirect,
		HTTP3Ingress:    http3Server,
	})
	if err != nil {
		return nil, fmt.Errorf("assembly lifecycle: %w", err)
	}
	var carrierDone <-chan error
	if source, ok := spec.Carrier.(interface{ Done() <-chan error }); ok {
		carrierDone = source.Done()
	}
	return &Assembly{carrierDone: carrierDone, stop: make(chan struct{}), dataPlane: dataPlane, Gateway: gateway, Private: private, Redirect: redirect, HTTP3: http3Server, done: make(chan error, 1)}, nil
}

func (a *Assembly) Start(ctx context.Context) error {
	if a == nil || a.dataPlane == nil {
		return ErrProcessInvalid
	}
	if err := a.dataPlane.Start(ctx); err != nil {
		return err
	}
	if a.carrierDone != nil {
		go a.watchCarrier()
	}
	go a.watchServer("public ingress", a.Gateway)
	go a.watchServer("private ingress", a.Private)
	go a.watchServer("redirect ingress", a.Redirect)
	go a.watchHTTP3(a.HTTP3)
	return nil
}

func (a *Assembly) watchHTTP3(server *HTTP3Server) {
	var err error
	select {
	case err = <-server.Done():
	case <-a.stop:
		return
	}
	if err == nil {
		err = errors.New("child exited")
	}
	select {
	case a.done <- fmt.Errorf("HTTP/3 ingress: %w", err):
	default:
	}
}

func (a *Assembly) watchServer(name string, server *HTTPServer) {
	var err error
	select {
	case err = <-server.Done():
	case <-a.stop:
		return
	}
	if err == nil {
		err = errors.New("child exited")
	}
	select {
	case a.done <- fmt.Errorf("%s: %w", name, err):
	default:
	}
}

func (a *Assembly) Done() <-chan error { return a.done }

func (a *Assembly) Shutdown(ctx context.Context) error {
	if a == nil || a.dataPlane == nil {
		return nil
	}
	a.stopOnce.Do(func() { close(a.stop) })
	return a.dataPlane.Shutdown(ctx)
}

func (a *Assembly) watchCarrier() {
	var err error
	select {
	case err = <-a.carrierDone:
	case <-a.stop:
		return
	}
	if err == nil {
		err = errors.New("carrier listener exited")
	}
	select {
	case a.done <- fmt.Errorf("carrier listener: %w", err):
	default:
	}
}
