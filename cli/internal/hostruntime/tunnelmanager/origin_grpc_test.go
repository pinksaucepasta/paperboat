package tunnelmanager

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// A real grpc-go client reaches a real grpc-go server (the origin, h2c) through the agent's origin transport: the
// unary call and its grpc-status trailer cross it, and a streaming health Watch keeps delivering updates.
func TestOriginHTTPTransportCarriesARealGRPCServiceOverH2C(t *testing.T) {
	origin := grpc.NewServer()
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(origin, healthServer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go origin.Serve(listener)
	defer origin.Stop()

	route := hoststate.TunnelConfigRoute{ID: "route_grpc", Protocol: "http", OriginScheme: "h2c", OriginAddress: listener.Addr().String(), PreserveHost: true, TLSVerification: "not_applicable", ConnectTimeoutMs: 1000, IdleTimeoutMs: 5000, DesiredState: "active"}
	transport := (&OriginHTTPTransport{}).newGeneration()
	defer transport.CloseIdleConnections()
	// What the edge and carrier give the agent is an HTTP request; the agent's transport is the leg under test.
	front := httptest.NewUnstartedServer(&httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.URL.Scheme, request.Out.URL.Host = "http", request.In.Host
		},
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return transport.RoundTrip(request.Context(), route, request)
		}),
		FlushInterval: -1,
	})
	front.Config.Protocols = new(http.Protocols)
	front.Config.Protocols.SetUnencryptedHTTP2(true)
	front.Start()
	defer front.Close()

	connection, err := grpc.NewClient(front.Listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := healthpb.NewHealthClient(connection)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil || response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health Check through the agent transport = %v, %v", response, err)
	}
	watch, err := client.Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := watch.Recv(); err != nil || first.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("first Watch update = %v, %v", first, err)
	}
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	if next, err := watch.Recv(); err != nil || next.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("streamed Watch update = %v, %v (a stream must keep delivering through the transport)", next, err)
	}
}
