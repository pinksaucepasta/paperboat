package sync

import (
	"crypto/tls"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"net"
	"time"
)

type Server struct {
	grpcServer *grpc.Server
	listener   net.Listener
	done       chan error
}

func StartServer(addr string, service *Service, certificate tls.Certificate) (*Server, error) {
	if service == nil || len(certificate.Certificate) == 0 || certificate.PrivateKey == nil {
		return nil, errors.New("authenticated sync service and TLS certificate are required")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}})), grpc.MaxRecvMsgSize(64<<10), grpc.MaxSendMsgSize(1<<20), grpc.MaxConcurrentStreams(4), grpc.ConnectionTimeout(5*time.Second), grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: time.Minute, MaxConnectionAge: 5 * time.Minute, MaxConnectionAgeGrace: 5 * time.Second, Time: 30 * time.Second, Timeout: 5 * time.Second}))
	service.Register(server)
	s := &Server{grpcServer: server, listener: listener, done: make(chan error, 1)}
	go func() { s.done <- server.Serve(listener) }()
	return s, nil
}
func (s *Server) Addr() string       { return s.listener.Addr().String() }
func (s *Server) Done() <-chan error { return s.done }
func (s *Server) Stop()              { s.grpcServer.Stop(); _ = s.listener.Close() }

// GracefulStop bounds shutdown because persistent streams require client cooperation.
func (s *Server) GracefulStop() {
	done := make(chan struct{})
	go func() { s.grpcServer.GracefulStop(); close(done) }()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		s.grpcServer.Stop()
		<-done
	}
	_ = s.listener.Close()
}
