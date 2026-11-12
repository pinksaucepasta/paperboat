package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIngressAuthoritySelectsExactMatchedAlias(t *testing.T) {
	now := time.Now().UTC()
	decision := ingressAuthorityTestDecision(now)
	managed := decision
	managed.Binding.Hostname = "managed.paperboat.test"
	authority := &IngressAuthority{Client: &HTTPClient{}, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch, fetched: now, decisions: []connectorprotocol.IngressDecision{managed, decision}}
	rule := route.RouteRule{RouteID: decision.Binding.RouteID, TunnelID: decision.Binding.TunnelID, AccountID: decision.Binding.AccountID, ConnectorID: decision.ConnectorID, ConnectorSessionID: decision.SessionID, ConnectorProcessGeneration: decision.ProcessGeneration, ConfigGeneration: decision.ConfigGeneration, AssignmentGeneration: decision.AssignmentGeneration, RouteGeneration: decision.Binding.RouteGeneration, AccessMode: decision.Binding.Audience, ViewerPolicyGeneration: decision.PolicyGeneration, Hostname: decision.Binding.Hostname}
	got, err := authority.Resolve(context.Background(), rule)
	if err != nil || got.Binding.Hostname != rule.Hostname {
		t.Fatalf("matched alias not selected: hostname=%s err=%v", got.Binding.Hostname, err)
	}
	rule.Hostname = "unpublished.customer.test"
	if _, err := authority.Resolve(context.Background(), rule); !errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("unpublished alias accepted: %v", err)
	}
}

func ingressAuthorityTestDecision(now time.Time) connectorprotocol.IngressDecision {
	return connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_01", AccountID: "account_01", TunnelID: "tunnel_01", Lifecycle: connectorprotocol.TunnelDurable, ResourceGeneration: 1, RouteID: "route_01", RouteGeneration: 1, TargetID: "target_01", TargetGeneration: 1, HostID: "host_01", InstallationGeneration: 1, Audience: "public", ConnectionMethod: "edge", Protocol: "http", Hostname: "app.customer.test", PathPrefix: "/", OriginScheme: "http", OriginAddress: "127.0.0.1:8080", TLSVerification: "not_applicable", PublicationID: "publication_01", PublicationGeneration: 1}, DecisionID: "decision_01", PolicyGeneration: 1, EdgeNodeID: "edge_01", EdgeProcessEpoch: "epoch_0001", ConnectorID: "connector_01", SessionID: "session_live_01", ProcessGeneration: 1, ConfigGeneration: 7, AssignmentGeneration: 1, Action: "view", IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(9 * time.Second)}
}

func TestIngressAuthorityInvalidationRefreshesNewRouteAndFencesInFlightSnapshot(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		decisions := []connectorprotocol.IngressDecision{}
		if n == 1 {
			close(started)
			<-release
		} else {
			decisions = append(decisions, decision)
		}
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": decisions})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	snapshotDone := make(chan error, 1)
	go func() { _, err := authority.Snapshot(context.Background()); snapshotDone <- err }()
	<-started
	// Invalidation does not wait for the in-flight fetch, but that fetch (started before it) must
	// not satisfy it: the next reads fetch again and see the new route.
	authority.Invalidate()
	close(release)
	if err := <-snapshotDone; err != nil {
		t.Fatal(err)
	}
	rule := route.RouteRule{RouteID: decision.Binding.RouteID, TunnelID: decision.Binding.TunnelID, AccountID: decision.Binding.AccountID, ConnectorID: decision.ConnectorID, ConnectorSessionID: decision.SessionID, ConnectorProcessGeneration: decision.ProcessGeneration, ConfigGeneration: decision.ConfigGeneration, AssignmentGeneration: decision.AssignmentGeneration, RouteGeneration: decision.Binding.RouteGeneration, AccessMode: decision.Binding.Audience, ViewerPolicyGeneration: decision.PolicyGeneration, Hostname: decision.Binding.Hostname}
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := authority.Resolve(context.Background(), rule); err != nil {
				t.Errorf("first promoted route request: %v", err)
			}
		}()
	}
	wait.Wait()
	if calls.Load() != 2 {
		t.Fatalf("refreshes=%d want 2", calls.Load())
	}
	rule.Hostname = "unpublished.customer.test"
	if _, err := authority.Resolve(context.Background(), rule); !errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("unknown alias: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal("unknown alias triggered extra fetch")
	}
}

func TestIngressAuthorityRefreshesColdPreviewWithoutRequestReplay(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "ready", false: "denied"}[allowed], func(t *testing.T) {
			now := time.Now().UTC()
			d := ingressAuthorityTestDecision(now)
			d.Binding.Lifecycle = connectorprotocol.TunnelEphemeral
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				decisions := []connectorprotocol.IngressDecision{}
				if allowed {
					decisions = append(decisions, d)
				}
				json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": decisions})
			}))
			defer server.Close()
			client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			a := &IngressAuthority{Client: client, NodeID: d.EdgeNodeID, ProcessEpoch: d.EdgeProcessEpoch, fetched: now}
			open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: d.Binding.AccountID, TunnelID: d.Binding.TunnelID, ConnectorID: d.ConnectorID, SessionID: d.SessionID, ProcessGeneration: d.ProcessGeneration, Generation: d.ConfigGeneration, RouteID: d.Binding.RouteID, RequestID: "preview-authority", Kind: "http"}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					got, err := a.ResolvePreview(context.Background(), d.Binding.PublicationID, d.Binding.Hostname, d.Binding.RouteGeneration, open)
					if allowed {
						if err != nil || got.Binding.PublicationID != d.Binding.PublicationID {
							t.Errorf("first ready preview request denied: %v", err)
						}
					} else if !errors.Is(err, connectorprotocol.ErrIngressDenied) {
						t.Errorf("absent publication accepted: %v", err)
					}
				}()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("control refreshes=%d, want one coalesced refresh", calls.Load())
			}
		})
	}
}

func TestIngressAuthorityRefreshesDecisionBeforeCacheInterval(t *testing.T) {
	for _, outcome := range []string{"renewed", "withdrawn", "unavailable"} {
		t.Run(outcome, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				d := ingressAuthorityTestDecision(time.Now().UTC())
				decisions := []connectorprotocol.IngressDecision{d}
				if n == 1 {
					d.ExpiresAt = time.Now().Add(75 * time.Millisecond)
					decisions[0] = d
				} else {
					switch outcome {
					case "withdrawn":
						decisions = nil
					case "unavailable":
						w.WriteHeader(503)
						return
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": decisions})
			}))
			defer server.Close()
			client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			authority := &IngressAuthority{Client: client, NodeID: "edge_01", ProcessEpoch: "epoch_0001"}
			before, err := authority.Snapshot(t.Context())
			if err != nil || len(before) != 1 {
				t.Fatalf("initial authority: %v", err)
			}
			<-time.After(time.Until(before[0].ExpiresAt) + time.Millisecond)
			after, err := authority.Snapshot(t.Context())
			if calls.Load() != 2 {
				t.Fatal("expired authority reused inside the refresh interval")
			}
			switch outcome {
			case "renewed":
				if err != nil || len(after) != 1 || !after[0].ExpiresAt.After(before[0].ExpiresAt) || after[0].Validate(time.Now().UTC()) != nil {
					t.Fatalf("renewed authority missing: %v", err)
				}
			case "withdrawn":
				if err != nil || len(after) != 0 {
					t.Fatalf("withdrawn authority retained: %v", err)
				}
			case "unavailable":
				// F21-17: while control is unreachable a public decision is renewed locally.
				if err != nil || len(after) != 1 || after[0].Validate(time.Now().UTC()) != nil {
					t.Fatalf("outage dropped public authority: %d %v", len(after), err)
				}
				if _, out := authority.OutOfSync(); !out {
					t.Fatal("outage not reported out of sync")
				}
			}
		})
	}
}

// F21-17: during a control-plane outage the edge keeps serving its last good public decisions and
// stops asking on every request; restricted decisions lapse at their expiry; the first good refresh
// after the outage replaces the set (a decision withdrawn meanwhile is gone).
func TestIngressAuthorityKeepsPublicDecisionsThroughAControlOutage(t *testing.T) {
	var calls atomic.Int32
	var down atomic.Bool
	public := ingressAuthorityTestDecision(time.Now().UTC())
	restricted := public
	restricted.DecisionID, restricted.Binding.Hostname, restricted.Binding.Audience = "decision_02", "team.customer.test", "team"
	restricted.PrincipalID, restricted.GrantID, restricted.GrantGeneration, restricted.MembershipGeneration = "principal_01", "grant_01", 1, 1
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if down.Load() {
			w.WriteHeader(503)
			return
		}
		now := time.Now().UTC()
		p, q := public, restricted
		p.IssuedAt, p.ExpiresAt = now, now.Add(150*time.Millisecond)
		q.IssuedAt, q.ExpiresAt = now, now.Add(150*time.Millisecond)
		decisions := []connectorprotocol.IngressDecision{p, q}
		if calls.Load() > 3 { // after the outage the restricted route was withdrawn
			decisions = decisions[:1]
		}
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": decisions})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: public.EdgeNodeID, ProcessEpoch: public.EdgeProcessEpoch}
	if got, err := authority.Snapshot(t.Context()); err != nil || len(got) != 2 {
		t.Fatalf("initial snapshot: %d %v", len(got), err)
	}
	down.Store(true)
	time.Sleep(200 * time.Millisecond) // both decisions expired
	got, err := authority.Snapshot(t.Context())
	if err != nil || len(got) != 1 || got[0].Binding.Audience != "public" || got[0].Validate(time.Now().UTC()) != nil {
		t.Fatalf("outage: %d decisions %v, want the public one renewed", len(got), err)
	}
	if _, out := authority.OutOfSync(); !out {
		t.Fatal("outage not reported out of sync")
	}
	during := calls.Load()
	for i := 0; i < 50; i++ {
		if _, err := authority.Snapshot(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if extra := calls.Load() - during; extra > 1 {
		t.Fatalf("%d fetches during backoff, want at most one", extra)
	}
	down.Store(false)
	calls.Store(3)
	authority.mu.Lock()
	authority.retryAt = time.Time{}
	authority.mu.Unlock()
	authority.Invalidate()
	got, err = authority.Snapshot(t.Context())
	if err != nil || len(got) != 1 || got[0].Binding.Audience != "public" {
		t.Fatalf("after the outage: %d %v", len(got), err)
	}
	if _, out := authority.OutOfSync(); out {
		t.Fatal("still out of sync after a good refresh")
	}
}

// F21-38: an edge that holds the current version gets fresh times only. An idle edge with 200
// decisions pulling every refresh interval for a minute moves a small fraction of the full bytes,
// and a changed set is fetched whole.
func TestIngressAuthorityAsksForAVersionAndIdleBytesStaySmall(t *testing.T) {
	base := ingressAuthorityTestDecision(time.Now().UTC())
	var bytesSent, fullBytes atomic.Int64
	var fulls, unchanged atomic.Int32
	changed := atomic.Bool{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Version string `json:"version"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		now := time.Now().UTC()
		decisions := make([]connectorprotocol.IngressDecision, 200)
		for i := range decisions {
			d := base
			d.DecisionID, d.Binding.Hostname, d.Binding.RouteID = "decision_"+strings.Repeat("x", 8)+string(rune('a'+i%26))+string(rune('a'+i/26)), "h"+string(rune('a'+i%26))+string(rune('a'+i/26))+".customer.test", "route_"+string(rune('a'+i%26))+string(rune('a'+i/26))
			d.IssuedAt, d.ExpiresAt = now, now.Add(8*time.Second+time.Duration(i)*time.Millisecond)
			decisions[i] = d
		}
		if changed.Load() {
			decisions = decisions[:199]
		}
		version := connectorprotocol.IngressVersion(decisions, nil, nil, nil, nil)
		body := map[string]any{"complete": true, "version": version, "decisions": decisions}
		if in.Version == version {
			expires := make([]time.Time, len(decisions))
			for i, d := range decisions {
				expires[i] = d.ExpiresAt
			}
			body = map[string]any{"complete": true, "unchanged": true, "version": version, "issued_at": now, "expires_at": expires}
			unchanged.Add(1)
		} else {
			fulls.Add(1)
		}
		encoded, _ := json.Marshal(body)
		bytesSent.Add(int64(len(encoded)))
		if in.Version != version {
			fullBytes.Store(int64(len(encoded)))
		}
		w.Write(encoded)
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: base.EdgeNodeID, ProcessEpoch: base.EdgeProcessEpoch}
	for i := 0; i < 12; i++ { // a minute of refresh intervals
		if got, err := authority.snapshot(t.Context(), true); err != nil || len(got) != 200 {
			t.Fatalf("pull %d: %d %v", i, len(got), err)
		}
	}
	if fulls.Load() != 1 || unchanged.Load() != 11 {
		t.Fatalf("%d full and %d unchanged answers, want 1 and 11", fulls.Load(), unchanged.Load())
	}
	if perMinute, budget := bytesSent.Load(), fullBytes.Load()*3; perMinute > budget {
		t.Fatalf("idle edge moved %d bytes in a minute, budget %d", perMinute, budget)
	}
	got, _ := authority.snapshot(t.Context(), true)
	if time.Until(got[0].ExpiresAt) < 7*time.Second {
		t.Fatal("unchanged answer did not refresh the decision times")
	}
	changed.Store(true)
	if got, err := authority.snapshot(t.Context(), true); err != nil || len(got) != 199 || fulls.Load() != 2 {
		t.Fatalf("changed set: %d decisions, %d full answers, %v", len(got), fulls.Load(), err)
	}
}

// F21-17: with the background loop running, an accept never waits on the control plane. While it
// hangs, the last decisions are served past their expiry (public renewed locally, restricted
// lapsed), and only a newer snapshot revokes one.
func TestIngressAuthorityAcceptNeverWaitsOnAHangingControlPlane(t *testing.T) {
	var hang, withdrawn atomic.Bool
	release := make(chan struct{})
	public := ingressAuthorityTestDecision(time.Now().UTC())
	restricted := public
	restricted.DecisionID, restricted.Binding.Hostname, restricted.Binding.Audience = "decision_02", "team.customer.test", "team"
	restricted.PrincipalID, restricted.GrantID, restricted.GrantGeneration, restricted.MembershipGeneration = "principal_01", "grant_01", 1, 1
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		now := time.Now().UTC()
		p, q := public, restricted
		p.IssuedAt, p.ExpiresAt = now, now.Add(300*time.Millisecond)
		q.IssuedAt, q.ExpiresAt = now, now.Add(300*time.Millisecond)
		decisions := []connectorprotocol.IngressDecision{p, q}
		if withdrawn.Load() {
			decisions = nil
		}
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": decisions})
	}))
	defer server.Close()
	defer close(release)
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: public.EdgeNodeID, ProcessEpoch: public.EdgeProcessEpoch}
	if err := authority.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer authority.Shutdown(context.Background())
	until := time.Now().Add(3 * time.Second)
	for {
		if got, err := authority.Snapshot(t.Context()); err == nil && len(got) == 2 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("background loop never fetched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	hang.Store(true)
	time.Sleep(400 * time.Millisecond) // both decisions are past their expiry
	for i := 0; i < 20; i++ {
		started := time.Now()
		got, err := authority.Snapshot(t.Context())
		if took := time.Since(started); took > 100*time.Millisecond {
			t.Fatalf("accept waited %s on the control plane", took)
		}
		if err != nil || len(got) != 1 || got[0].Binding.Audience != "public" || got[0].Validate(time.Now().UTC()) != nil {
			t.Fatalf("expired set: %d decisions, %v; want the public one served", len(got), err)
		}
	}
	// A newer snapshot is the only thing that revokes it.
	hang.Store(false)
	withdrawn.Store(true)
	until = time.Now().Add(8 * time.Second)
	for {
		got, _ := authority.Snapshot(t.Context())
		if len(got) == 0 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("withdrawn decision still served after the control plane recovered")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// After a handover the server issues the replacement's preview decision a moment after the host's carrier attaches.
// A miss is remembered for the refresh interval; during the takeover grace it is asked again after 500 ms, so the
// late decision is found within about a second, not after 5 s (anvit 2026-11-09: one request per swap waited 5 s).
func TestIngressAuthorityPreviewMissIsRecheckedDuringTakeoverGrace(t *testing.T) {
	for _, grace := range []bool{true, false} {
		t.Run(map[bool]string{true: "in grace", false: "no takeover"}[grace], func(t *testing.T) {
			now := time.Now().UTC()
			d := ingressAuthorityTestDecision(now)
			d.Binding.Lifecycle = connectorprotocol.TunnelEphemeral
			var calls atomic.Int32
			ready := time.Now().Add(700 * time.Millisecond)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				decisions := []connectorprotocol.IngressDecision{}
				if time.Now().After(ready) {
					decisions = append(decisions, d)
				}
				json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": decisions})
			}))
			defer server.Close()
			client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			a := &IngressAuthority{Client: client, NodeID: d.EdgeNodeID, ProcessEpoch: d.EdgeProcessEpoch, fetched: now}
			if grace {
				a.Grace = func() time.Duration { return 5 * time.Second }
			}
			open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: d.Binding.AccountID, TunnelID: d.Binding.TunnelID, ConnectorID: d.ConnectorID, SessionID: d.SessionID, ProcessGeneration: d.ProcessGeneration, Generation: d.ConfigGeneration, RouteID: d.Binding.RouteID, RequestID: "preview-authority", Kind: "http"}
			deadline := time.Now().Add(1500 * time.Millisecond)
			var found bool
			for time.Now().Before(deadline) && !found {
				_, err := a.ResolvePreview(context.Background(), d.Binding.PublicationID, d.Binding.Hostname, d.Binding.RouteGeneration, open)
				found = err == nil
				time.Sleep(50 * time.Millisecond)
			}
			if found != grace {
				t.Fatalf("decision found within 1.5 s = %v, want %v (control calls %d)", found, grace, calls.Load())
			}
		})
	}
}

// Held connections of many names must not each cost their own forced fetch: during the grace one fetch answers every
// name, so 20 publications rechecked every 50 ms for 1.2 s cause a handful of fetches, not dozens; and one name that
// keeps missing is asked again less and less often.
func TestIngressAuthorityPreviewMissesInGraceShareFetchesAndBackOff(t *testing.T) {
	now := time.Now().UTC()
	d := ingressAuthorityTestDecision(now)
	d.Binding.Lifecycle = connectorprotocol.TunnelEphemeral
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{}})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	a := &IngressAuthority{Client: client, NodeID: d.EdgeNodeID, ProcessEpoch: d.EdgeProcessEpoch, fetched: now, Grace: func() time.Duration { return 10 * time.Second }}
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: d.Binding.AccountID, TunnelID: d.Binding.TunnelID, ConnectorID: d.ConnectorID, SessionID: d.SessionID, ProcessGeneration: d.ProcessGeneration, Generation: d.ConfigGeneration, RouteID: d.Binding.RouteID, RequestID: "preview-authority", Kind: "http"}
	for end := time.Now().Add(1200 * time.Millisecond); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		for i := 0; i < 20; i++ {
			_, _ = a.ResolvePreview(context.Background(), "publication_"+strconv.Itoa(i), "n"+strconv.Itoa(i)+".preview.example.test", d.Binding.RouteGeneration, open)
		}
	}
	if got := calls.Load(); got > 6 {
		t.Fatalf("%d control fetches for 20 missing names in 1.2 s, want a handful (<= 6)", got)
	}
	if ttl := a.previewMissTTL(previewMissMaxStreak); ttl != 2*time.Second || a.previewMissTTL(0) != previewRecheckInGrace || a.previewMissTTL(1) != previewRecheckInGrace || a.previewMissTTL(2) != time.Second {
		t.Fatalf("backoff %v %v %v %v, want 0.5s 0.5s 1s 2s", a.previewMissTTL(0), a.previewMissTTL(1), a.previewMissTTL(2), ttl)
	}
}

// The held snapshot admits a preview publication by hostname while its decision stands: an ephemeral public one always
// (it is renewed locally until a newer snapshot ends it), a restricted one until it expires; a durable tunnel's decision
// or an unknown name is not a preview admission.
func TestAdmitsPreviewByHostname(t *testing.T) {
	now := time.Now()
	decision := func(host, audience string, lifecycle string, expires time.Time) connectorprotocol.IngressDecision {
		var d connectorprotocol.IngressDecision
		d.Binding.Hostname, d.Binding.Audience, d.Binding.Lifecycle, d.ExpiresAt = host, audience, lifecycle, expires
		return d
	}
	a := &IngressAuthority{decisions: []connectorprotocol.IngressDecision{
		decision("public.preview.example.test", "public", connectorprotocol.TunnelEphemeral, now.Add(-time.Hour)),
		decision("team.preview.example.test", "team", connectorprotocol.TunnelEphemeral, now.Add(time.Hour)),
		decision("lapsed.preview.example.test", "team", connectorprotocol.TunnelEphemeral, now.Add(-time.Minute)),
		decision("tunnel.tunnels.example.test", "public", connectorprotocol.TunnelDurable, now.Add(time.Hour)),
	}}
	for host, want := range map[string]bool{
		"public.preview.example.test": true, "team.preview.example.test": true, "lapsed.preview.example.test": false,
		"tunnel.tunnels.example.test": false, "unknown.preview.example.test": false, "": false,
	} {
		if got := a.AdmitsPreview(host); got != want {
			t.Errorf("AdmitsPreview(%q) = %v, want %v", host, got, want)
		}
	}
	if (*IngressAuthority)(nil).AdmitsPreview("public.preview.example.test") {
		t.Error("a missing authority admitted a preview")
	}
}

// A decision issued to another edge is that edge's preview: this edge answers 404 for it as before, not 503.
func TestAdmitsPreviewOnlyForDecisionsIssuedToThisEdge(t *testing.T) {
	mine, other := connectorprotocol.IngressDecision{EdgeNodeID: "edge_1"}, connectorprotocol.IngressDecision{EdgeNodeID: "edge_2"}
	for _, d := range []*connectorprotocol.IngressDecision{&mine, &other} {
		d.Binding.Audience, d.Binding.Lifecycle = "public", connectorprotocol.TunnelEphemeral
	}
	mine.Binding.Hostname, other.Binding.Hostname = "mine.preview.example.test", "theirs.preview.example.test"
	a := &IngressAuthority{NodeID: "edge_1", decisions: []connectorprotocol.IngressDecision{mine, other}}
	if !a.AdmitsPreview("mine.preview.example.test") || a.AdmitsPreview("theirs.preview.example.test") {
		t.Fatalf("mine admitted %v, other edge's admitted %v", a.AdmitsPreview("mine.preview.example.test"), a.AdmitsPreview("theirs.preview.example.test"))
	}
}

// Reads of the held snapshot do not queue behind each other or behind a scan: AdmitsPreview takes the read lock only.
func TestAdmitsPreviewDoesNotTakeTheWriteLock(t *testing.T) {
	a := &IngressAuthority{}
	a.mu.RLock()
	defer a.mu.RUnlock()
	done := make(chan struct{})
	go func() { a.AdmitsPreview("any.preview.example.test"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AdmitsPreview blocked while another reader held the lock")
	}
}

// The first answers of a freshly started server take about 2 s (the 2026-11-12 server swap: 1.7-2.0 s against
// the edge's 2 s deadline). A slow answer must still be taken, not counted as a control failure: the fetch has
// its own timeout, longer than the control timeout.
func TestIngressAuthorityTakesAnAnswerSlowerThanTheControlTimeout(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{decision}})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	decisions, err := authority.Snapshot(context.Background())
	if err != nil || len(decisions) != 1 {
		t.Fatalf("a 1.5 s answer to a 1 s control timeout was refused: %d decisions, %v", len(decisions), err)
	}
}

// Warm closes with the first successful fetch only, never with a failed one: a replacement edge process
// must not take traffic before it knows its routes, or it denies a route it has not loaded.
func TestIngressAuthorityIsWarmOnlyAfterTheFirstSuccessfulFetch(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "control down", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{decision}})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	if _, err := authority.Snapshot(context.Background()); err == nil {
		t.Fatal("the failing first fetch succeeded")
	}
	select {
	case <-authority.Warm():
		t.Fatal("warm after a failed fetch")
	default:
	}
	authority.retryAt = time.Time{}
	if _, err := authority.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-authority.Warm():
	default:
		t.Fatal("not warm after the first successful fetch")
	}
}

// A caller with a short deadline that starts the shared fetch gives up alone: the fetch keeps its own bound,
// so a slow answer lands and no control failure is counted (server deploys 2026-11-12: a 2 s public TCP
// reconcile cancelled 2-3 s desired-state answers, 499 on the server, a control failure on the edge).
func TestIngressAuthoritySharedFetchOutlivesTheCallerThatStartedIt(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(600 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{decision}})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := authority.Snapshot(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short caller: %v, want its own deadline", err)
	}
	decisions, err := authority.Snapshot(context.Background())
	if err != nil || len(decisions) != 1 || calls.Load() != 1 {
		t.Fatalf("after the short caller: %d decisions, %v, %d fetches; want the one shared answer", len(decisions), err, calls.Load())
	}
	if _, out := authority.OutOfSync(); out || authority.failures != 0 {
		t.Fatalf("a caller's deadline counted as a control failure (failures=%d)", authority.failures)
	}
}

// A caller fenced by Invalidate whose deadline passes during a slow refresh gets the held decisions, not an error:
// a TLS handshake that got the error was dropped (edge handover 2026-11-12 22:28Z: a preview closed after 2 s).
func TestIngressAuthorityFencedCallerGetsTheHeldSetWhenTheRefreshIsSlow(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			time.Sleep(600 * time.Millisecond)
		}
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{decision}})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	if got, err := authority.Snapshot(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("first fetch: %d decisions, %v", len(got), err)
	}
	authority.Invalidate()
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	got, err := authority.Snapshot(short)
	if err != nil || len(got) != 1 {
		t.Fatalf("fenced caller during a slow refresh: %d decisions, %v; want the held one", len(got), err)
	}
}
