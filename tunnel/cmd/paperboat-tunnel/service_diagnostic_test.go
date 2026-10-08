package main

import (
	"errors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	edgeruntime "github.com/pinksaucepasta/paperboat-tunnel/internal/runtime"
	"strings"
	"testing"
)

func TestServiceDiagnosticExcludesPrivateErrorData(t *testing.T) {
	err := &edgeruntime.StartError{Component: "*runtime.Assembly", Err: errors.Join(&edgeruntime.StartError{Component: "*runtime.NodeWorker", Err: &control.RequestFailure{Path: "/v1/nodes/register", Status: 401, Category: "http_status", Err: control.ErrControlUnavailable}}, errors.New("SECRET credential private body"))}
	got := serviceDiagnostic(err)
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "*runtime.Assembly/*runtime.NodeWorker") || !strings.Contains(got, "http_status=401") || !strings.Contains(got, "control_path=/v1/nodes/register") {
		t.Fatalf("diagnostic = %q", got)
	}
}
