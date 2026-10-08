package configsync

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

// The operation owns its connection from TCP dial through pack consumption.
// go-git's default SSH dialer does not carry CloneContext into the handshake.
type repositorySSHAuth struct {
	gitssh.AuthMethod
	ctx context.Context
}

func (*repositorySSHAuth) String() string { return "repository SSH credentials" }

type repositorySSHTransport struct{}

func (repositorySSHTransport) NewUploadPackSession(ep *transport.Endpoint, a transport.AuthMethod) (transport.UploadPackSession, error) {
	return newRepositorySSHSession(ep, a, transport.UploadPackServiceName)
}
func (repositorySSHTransport) NewReceivePackSession(ep *transport.Endpoint, a transport.AuthMethod) (transport.ReceivePackSession, error) {
	return newRepositorySSHSession(ep, a, transport.ReceivePackServiceName)
}

type repositoryContextCloser struct {
	closer io.Closer
	stop   func() bool
	once   sync.Once
	err    error
}

func newRepositoryContextCloser(ctx context.Context, c io.Closer) *repositoryContextCloser {
	r := &repositoryContextCloser{closer: c}
	r.stop = context.AfterFunc(ctx, r.closeUnderlying)
	return r
}
func (r *repositoryContextCloser) Close() error {
	r.stop()
	r.closeUnderlying()
	return r.err
}

// Both owners join the same close and retain its original outcome.
func (r *repositoryContextCloser) closeUnderlying() {
	r.once.Do(func() { r.err = r.closer.Close() })
}

type repositorySSHSession struct {
	ctx              context.Context
	cancel           context.CancelFunc
	stop             func() bool
	conn             net.Conn
	client           *ssh.Client
	session          *ssh.Session
	in               io.WriteCloser
	out              io.Reader
	refs             *packp.AdvRefs
	receive, packRun bool
	once             sync.Once
	closeErr         error
}

func newRepositorySSHSession(ep *transport.Endpoint, a transport.AuthMethod, service string) (result *repositorySSHSession, err error) {
	auth, ok := a.(*repositorySSHAuth)
	if !ok || ep.Protocol != "ssh" || ep.Proxy.URL != "" {
		return nil, transport.ErrInvalidAuthMethod
	}
	cfg, err := auth.ClientConfig()
	if err != nil {
		return nil, err
	}
	if cfg.HostKeyCallback == nil || len(cfg.HostKeyAlgorithms) == 0 {
		return nil, ErrRepositoryCredentials
	}
	ctx := auth.ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	}
	port := ep.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(ep.Host, strconv.Itoa(port))
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &repositorySSHSession{ctx: ctx, cancel: cancel, conn: raw, receive: service == transport.ReceivePackServiceName}
	s.stop = context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer func() {
		if err != nil {
			_ = raw.Close()
			if s.client != nil {
				_ = s.client.Close()
				_ = s.client.Wait()
			}
			s.stop()
			err = s.failure(ctx, err)
			cancel()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err = raw.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	cc, ch, requests, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		return nil, err
	}
	s.client = ssh.NewClient(cc, ch, requests)
	s.session, err = s.client.NewSession()
	if err != nil {
		return nil, err
	}
	s.in, err = s.session.StdinPipe()
	if err != nil {
		return nil, err
	}
	s.out, err = s.session.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s.session.Stderr = io.Discard
	// Single-quote the exact granted path, including shells treating ! specially.
	path := "'" + strings.NewReplacer("'", "'\\''", "!", "'\\!'").Replace(ep.Path) + "'"
	if err = s.session.Start(service + " " + path); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *repositorySSHSession) operation(ctx context.Context) func() {
	stop := context.AfterFunc(ctx, func() { _ = s.conn.Close() })
	return func() { stop() }
}
func (s *repositorySSHSession) failure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	// A socket deadline can fire before the context timer goroutine runs.
	for _, operation := range []context.Context{ctx, s.ctx} {
		if deadline, ok := operation.Deadline(); ok && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}
	return err
}
func (s *repositorySSHSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	return s.AdvertisedReferencesContext(s.ctx)
}
func (s *repositorySSHSession) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	stop := s.operation(ctx)
	defer stop()
	if s.refs != nil {
		return s.refs, nil
	}
	refs := packp.NewAdvRefs()
	err := refs.Decode(s.out)
	if errors.Is(err, packp.ErrEmptyAdvRefs) && s.receive {
		err = nil
	}
	if err != nil {
		_ = s.conn.Close()
		return nil, s.failure(ctx, err)
	}
	if !s.receive && refs.IsEmpty() {
		return nil, transport.ErrEmptyRemoteRepository
	}
	transport.FilterUnsupportedCapabilities(refs.Capabilities)
	s.refs = refs
	return refs, nil
}
func (s *repositorySSHSession) UploadPack(ctx context.Context, req *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	if req.IsEmpty() {
		return nil, transport.ErrEmptyUploadPackRequest
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if _, err := s.AdvertisedReferencesContext(ctx); err != nil {
		return nil, err
	}
	stop := s.operation(ctx)
	s.packRun = true
	failed := func(err error) (*packp.UploadPackResponse, error) {
		_ = s.conn.Close()
		stop()
		return nil, s.failure(ctx, err)
	}
	if err := req.UploadRequest.Encode(s.in); err != nil {
		return failed(err)
	}
	if err := req.UploadHaves.Encode(s.in, true); err != nil {
		return failed(err)
	}
	if err := pktline.NewEncoder(s.in).Encodef("done\n"); err != nil {
		return failed(err)
	}
	if err := s.in.Close(); err != nil {
		return failed(err)
	}
	res := packp.NewUploadPackResponse(req)
	if err := res.Decode(&repositorySSHResponse{Reader: s.out, s: s, stop: stop, ctx: ctx}); err != nil {
		return failed(err)
	}
	return res, nil
}

type repositorySSHResponse struct {
	io.Reader
	s    *repositorySSHSession
	stop func()
	ctx  context.Context
}

func (r *repositorySSHResponse) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	return n, r.s.failure(r.ctx, err)
}
func (r *repositorySSHResponse) Close() error { defer r.stop(); return r.s.Close() }
func (s *repositorySSHSession) ReceivePack(ctx context.Context, req *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	stop := s.operation(ctx)
	defer stop()
	if _, err := s.AdvertisedReferencesContext(ctx); err != nil {
		return nil, err
	}
	s.packRun = true
	if err := req.Encode(s.in); err != nil {
		_ = s.conn.Close()
		return nil, s.failure(ctx, err)
	}
	if err := s.in.Close(); err != nil {
		_ = s.conn.Close()
		return nil, s.failure(ctx, err)
	}
	if !req.Capabilities.Supports(capability.ReportStatus) {
		return nil, s.failure(ctx, s.Close())
	}
	r := s.out
	var demux *sideband.Demuxer
	if req.Capabilities.Supports(capability.Sideband64k) {
		demux = sideband.NewDemuxer(sideband.Sideband64k, r)
	} else if req.Capabilities.Supports(capability.Sideband) {
		demux = sideband.NewDemuxer(sideband.Sideband, r)
	}
	if demux != nil {
		demux.Progress = req.Progress
		r = demux
	}
	report := packp.NewReportStatus()
	if err := report.Decode(r); err != nil {
		_ = s.conn.Close()
		return nil, s.failure(ctx, err)
	}
	if err := report.Error(); err != nil {
		return report, err
	}
	return report, s.failure(ctx, s.Close())
}
func (s *repositorySSHSession) Close() error {
	s.once.Do(func() {
		if !s.packRun {
			_, _ = s.in.Write(pktline.FlushPkt)
		}
		_ = s.in.Close()
		s.closeErr = s.session.Wait()
		_ = s.client.Close()
		_ = s.client.Wait()
		s.stop()
		s.closeErr = s.failure(context.Background(), s.closeErr)
		s.cancel()
	})
	return s.closeErr
}
