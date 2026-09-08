package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

var ErrHTTP3Unavailable = errors.New("HTTP/3 connector establishment timed out")

const httpCarrierPath = "/_paperboat/connector-v1"

// NewHTTPNetworkDialer carries the existing bounded yamux carrier through a
// full-duplex HTTP CONNECT over explicit HTTP3 and HTTP2 transports.
func NewHTTPNetworkDialer(config NetworkDialerConfig) DataCarrierDialer {
	return func(ctx context.Context, request DataCarrierDialRequest) (DataCarrierDialResult, error) {
		var endpoint DataCarrierEndpointConfig
		var h3 bool
		switch request.Transport {
		case HTTP3:
			endpoint, h3 = config.QUIC, true
		case HTTP2:
			endpoint = config.TCPMux
		default:
			return DataCarrierDialResult{}, newTransportDialError(request.Transport, ErrInvalidDataCarrierEndpoint)
		}
		attemptCtx := ctx
		cancelAttempt := func() {}
		if h3 {
			attemptCtx, cancelAttempt = context.WithTimeout(ctx, 2*time.Second)
		}
		link, identity, err := dialHTTPCarrier(attemptCtx, endpoint, h3)
		cancelAttempt()
		if h3 && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return DataCarrierDialResult{}, &TransportDialError{Transport: request.Transport, Err: ErrHTTP3Unavailable, Fallback: true}
		}
		if err != nil {
			return DataCarrierDialResult{}, newTransportDialError(request.Transport, err)
		}
		if request.Identity != (DataCarrierIdentity{}) && identity != request.Identity {
			_ = link.Close()
			return DataCarrierDialResult{}, newTransportDialError(request.Transport, ErrDataCarrierAdmission)
		}
		return DataCarrierDialResult{Link: link, PeerIdentity: identity, Transport: request.Transport, EdgeID: request.EdgeID, FailureDomain: request.FailureDomain}, nil
	}
}

func dialHTTPCarrier(ctx context.Context, endpoint DataCarrierEndpointConfig, h3 bool) (io.ReadWriteCloser, DataCarrierIdentity, error) {
	if ctx == nil {
		return nil, DataCarrierIdentity{}, ErrInvalidDataCarrierEndpoint
	}
	if err := endpoint.validateClient(); err != nil {
		return nil, DataCarrierIdentity{}, err
	}
	tlsConfig, err := clientTLSConfig(endpoint.TLS, endpoint.Address)
	if err != nil {
		return nil, DataCarrierIdentity{}, err
	}
	var transport carrierRoundTripper
	if h3 {
		tlsConfig.NextProtos = []string{"h3"}
		quicConfig := defaultQUICConfig()
		quicConfig.MaxIncomingUniStreams = 3
		rt := &http3.Transport{TLSClientConfig: tlsConfig, QUICConfig: quicConfig}
		transport = carrierRoundTripper{RoundTripper: rt, close: rt.Close}
	} else {
		tlsConfig.NextProtos = []string{"h2"}
		rt := &http2.Transport{TLSClientConfig: tlsConfig}
		transport = carrierRoundTripper{RoundTripper: rt, close: func() error { rt.CloseIdleConnections(); return nil }}
	}
	requestCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopStagingCancel := context.AfterFunc(ctx, cancel)
	defer stopStagingCancel()
	reader, writer := io.Pipe()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodConnect, "https://"+endpoint.Address+httpCarrierPath, reader)
	if err != nil {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		_ = transport.close()
		return nil, DataCarrierIdentity{}, err
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		_ = transport.close()
		if ctx.Err() != nil {
			return nil, DataCarrierIdentity{}, ctx.Err()
		}
		return nil, DataCarrierIdentity{}, fmt.Errorf("%w: HTTP carrier dial: %w", ErrDataCarrierTLS, err)
	}
	wantALPN := "h2"
	if h3 {
		wantALPN = "h3"
	}
	if resp.StatusCode != http.StatusOK || resp.TLS == nil || resp.TLS.NegotiatedProtocol != wantALPN {
		_ = resp.Body.Close()
		cancel()
		_ = writer.Close()
		_ = transport.close()
		return nil, DataCarrierIdentity{}, fmt.Errorf("%w: CONNECT status %d or invalid ALPN", ErrDataCarrierTLS, resp.StatusCode)
	}
	identity, err := bindPeer(endpoint, *resp.TLS)
	if err != nil {
		_ = resp.Body.Close()
		cancel()
		_ = writer.Close()
		_ = transport.close()
		return nil, DataCarrierIdentity{}, err
	}
	if !stopStagingCancel() {
		_ = resp.Body.Close()
		cancel()
		_ = writer.Close()
		_ = transport.close()
		return nil, DataCarrierIdentity{}, ctx.Err()
	}
	return &httpCarrierLink{reader: resp.Body, writer: writer, cancel: cancel, closeTransport: transport.close}, identity, nil
}

type carrierRoundTripper struct {
	http.RoundTripper
	close func() error
}

type httpCarrierLink struct {
	reader         io.ReadCloser
	writer         *io.PipeWriter
	cancel         context.CancelFunc
	closeTransport func() error
	once           sync.Once
	err            error
}

func (l *httpCarrierLink) Read(p []byte) (int, error)  { return l.reader.Read(p) }
func (l *httpCarrierLink) Write(p []byte) (int, error) { return l.writer.Write(p) }
func (l *httpCarrierLink) Close() error {
	l.once.Do(func() {
		l.cancel()
		if err := l.writer.Close(); err != nil {
			l.err = err
		}
		if err := l.reader.Close(); err != nil && l.err == nil {
			l.err = err
		}
		if err := l.closeTransport(); err != nil && l.err == nil {
			l.err = err
		}
	})
	return l.err
}
