package edgehttp

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type recordingUsage struct {
	mu       sync.Mutex
	records  []string
	exhaust  map[string]func()
	recorded map[string][2]uint64
}

func (u *recordingUsage) SubscribeQuota(authority string, _ time.Time, cancel func()) func() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.exhaust == nil {
		u.exhaust = map[string]func(){}
	}
	u.exhaust[authority] = cancel
	return func() {
		u.mu.Lock()
		delete(u.exhaust, authority)
		u.mu.Unlock()
	}
}

func (u *recordingUsage) Record(environment, route string, revision uint64, ingress, egress uint64, authority string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.recorded == nil {
		u.recorded = map[string][2]uint64{}
	}
	key := authority + "|" + environment + "|" + route
	got := u.recorded[key]
	u.recorded[key] = [2]uint64{got[0] + ingress, got[1] + egress}
	return nil
}

func TestRuntimeSessionBytesAreRecordedUnderItsAuthorityAndCutAtTheCap(t *testing.T) {
	usage := &recordingUsage{}
	edgeSide, hostSide := net.Pipe()
	defer hostSide.Close()
	ctx := withRuntimeUsage(context.Background(), runtimeUsageBinding{Authority: "session_a", Environment: "env_1", IssuedAt: time.Now()})
	binding, ok := runtimeUsageFrom(ctx)
	if !ok {
		t.Fatal("runtime usage binding not carried in the request context")
	}
	binding.Route, binding.Revision = "route_runtime", 4
	stream := meterRuntimeStream(usage, binding, edgeSide)
	go func() { buffer := make([]byte, 64); n, _ := hostSide.Read(buffer); _, _ = hostSide.Write(buffer[:n]) }()
	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	if got := usage.recorded["session_a|env_1|route_runtime"]; got != [2]uint64{5, 5} {
		t.Fatalf("recorded under the session authority = %v, want 5 in / 5 out", got)
	}
	usage.mu.Lock()
	cut := usage.exhaust["session_a"]
	usage.mu.Unlock()
	if cut == nil {
		t.Fatal("the stream did not subscribe to its authority's quota")
	}
	cut()
	if _, err := stream.Write([]byte("more")); !errors.Is(err, ErrIngressQuotaExhausted) {
		t.Fatalf("write after the allowance was exhausted: %v, want ErrIngressQuotaExhausted", err)
	}
	_ = stream.Close()
	usage.mu.Lock()
	_, still := usage.exhaust["session_a"]
	usage.mu.Unlock()
	if still {
		t.Fatal("closing the stream left its quota subscription")
	}
}

func TestRuntimeUsageNeedsAnAuthority(t *testing.T) {
	if _, ok := runtimeUsageFrom(withRuntimeUsage(context.Background(), runtimeUsageBinding{Environment: "env_1"})); ok {
		t.Fatal("a credential without an assignment id produced a usage binding")
	}
	if _, ok := runtimeUsageFrom(context.Background()); ok {
		t.Fatal("a request without a runtime credential produced a usage binding")
	}
}
