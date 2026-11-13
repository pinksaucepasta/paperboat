package main

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	edgeruntime "github.com/pinksaucepasta/paperboat/edge/internal/runtime"
	"net"
	"strings"
	"syscall"
	"testing"
)

// The shared projection (F21-39) reports the operational cause when a 4xx sits beside an independent
// failure, and keeps the 401 as http_status; the private body and the request path never appear.
func TestServiceDiagnosticExcludesPrivateErrorData(t *testing.T) {
	err := &edgeruntime.StartError{Component: "*runtime.Assembly", Err: errors.Join(&edgeruntime.StartError{Component: "*runtime.NodeWorker", Err: &control.RequestFailure{Path: "/v1/nodes/register", Status: 401, Category: "http_status", Err: control.ErrControlUnavailable}}, errors.New("SECRET credential private body"))}
	got := serviceDiagnostic(err)
	if strings.Contains(got, "SECRET") || strings.Contains(got, "/v1/nodes/register") || !strings.Contains(got, "cause=control_unavailable") || !strings.Contains(got, "http_status=401") {
		t.Fatalf("diagnostic = %q", got)
	}
}

// A start failure names the components it passed through and the text of the leaf error, so the
// journal says which listener or component refused to start (oracle, 2026-11-05).
func TestServiceDiagnosticNamesTheFailingComponentAndItsError(t *testing.T) {
	bind := &net.OpError{Op: "listen", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 221), Port: 443}, Err: syscall.EADDRINUSE}
	err := errors.Join(&edgeruntime.StartError{Component: "*runtime.DataPlane", Err: errors.Join(&edgeruntime.StartError{Component: "*runtime.HTTPServer", Err: bind}, nil)}, nil)
	got := serviceDiagnostic(err)
	for _, want := range []string{"components=*runtime.DataPlane>*runtime.HTTPServer", `detail="listen tcp 10.0.0.221:443: address already in use"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic = %q, want %q", got, want)
		}
	}
	plain := serviceDiagnostic(&edgeruntime.StartError{Component: "*runtime.Usage", Err: errors.New("usage key file is empty")})
	if !strings.Contains(plain, `detail="usage key file is empty"`) {
		t.Fatalf("diagnostic = %q", plain)
	}
}

func TestServiceDiagnosticNeverPrintsSecretLookingTextOrUnknownErrorTypes(t *testing.T) {
	for _, leaf := range []error{errors.New("bad bearer abc123"), errors.New("-----BEGIN PRIVATE KEY-----"), &control.RequestFailure{Path: "/v1/x", Status: 500, Err: errors.New("response body: hello")}} {
		got := serviceDiagnostic(&edgeruntime.StartError{Component: "*runtime.NodeWorker", Err: leaf})
		if strings.Contains(got, "abc123") || strings.Contains(got, "PRIVATE") || strings.Contains(got, "hello") || strings.Contains(got, "/v1/x") {
			t.Fatalf("diagnostic leaks %v: %q", leaf, got)
		}
	}
}
