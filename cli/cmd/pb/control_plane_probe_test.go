package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
)

type probeResolver struct{ calls []resolver.ConnectRequest }

// Resolve holds the request like a blackholed API until it is cancelled, unless it is told Paperboat is down.
func (r *probeResolver) Resolve(ctx context.Context, request resolver.ConnectRequest) (resolver.ConnectInfo, error) {
	r.calls = append(r.calls, request)
	if request.ControlPlaneUnavailable || request.Machine == "healthy" {
		return resolver.ConnectInfo{Machine: request.Machine}, nil
	}
	<-ctx.Done()
	return resolver.ConnectInfo{}, ctx.Err()
}

type blockedAuth struct{ release chan struct{} }

func (a blockedAuth) Credential() (config.Credential, error) {
	<-a.release
	return config.Credential{AccessToken: "late"}, nil
}

// withDaemonSnapshot is the context of a host whose daemon answers with snapshot.
func withDaemonSnapshot(t *testing.T, snapshot localapi.Snapshot) context.Context {
	t.Helper()
	return withHostDependencies(t.Context(), func(d *hostDependencies) {
		d.warmSnapshot = func(context.Context) (localapi.Snapshot, error) { return snapshot, nil }
	})
}

// A control plane that refuses connections ends the descriptor request at once and resolves from the host's own
// rules; a token refresh stuck behind it is not waited for.
func TestControlPlaneProbeDownCutsTheRequestAndTheCredentialWait(t *testing.T) {
	ctx := withDaemonSnapshot(t, localapi.Snapshot{})
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	probe := startControlPlaneProbe(ctx, dead.URL)
	auth := blockedAuth{release: make(chan struct{})}
	defer close(auth.release)
	started := time.Now()
	cred, err := probe.credential(auth)
	if err != nil || cred.AccessToken != "" || !probe.Down() {
		t.Fatalf("credential = %+v, %v, down = %v", cred, err, probe.Down())
	}
	r := &probeResolver{}
	info, err := resolveWhileProbing(ctx, r, resolver.ConnectRequest{Machine: "m", Credential: config.Credential{AccessToken: "t"}}, probe)
	if err != nil || info.Machine != "m" || time.Since(started) > 2*time.Second {
		t.Fatalf("info = %+v, err = %v after %s", info, err, time.Since(started))
	}
	if last := r.calls[len(r.calls)-1]; !last.ControlPlaneUnavailable || last.Credential.AccessToken != "" {
		t.Fatalf("offline request = %+v", last)
	}
}

// A healthy control plane is asked once, with the credential, and never told it is down.
func TestControlPlaneProbeUpResolvesOnlineOnce(t *testing.T) {
	ctx := withDaemonSnapshot(t, localapi.Snapshot{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer up.Close()
	probe := startControlPlaneProbe(ctx, up.URL)
	r := &probeResolver{}
	info, err := resolveWhileProbing(ctx, r, resolver.ConnectRequest{Machine: "healthy"}, probe)
	<-probe.done
	if err != nil || info.Machine != "healthy" || len(r.calls) != 1 || r.calls[0].ControlPlaneUnavailable || probe.Down() {
		t.Fatalf("info = %+v, err = %v, calls = %+v, down = %v", info, err, r.calls, probe.Down())
	}
}

// The daemon's own outage report is the verdict before any request is made.
func TestControlPlaneProbeTakesTheDaemonOutageReportAtOnce(t *testing.T) {
	ctx := withDaemonSnapshot(t, localapi.Snapshot{Health: []localapi.HealthItem{{Code: "control_plane_unavailable"}}})
	if !startControlPlaneProbe(ctx, "http://127.0.0.1:1").Down() {
		t.Fatal("daemon outage not taken as the verdict")
	}
}
