package runtime

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
)

// A route the server answers as removed, or leaves out of a full set, is logged with that reason
// (a preview went 404 right after it was ready with no trace of why, 2026-11-21).
func TestEndpointRemovalsAreLoggedWithReason(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	admission := func(route string) runtimewire.Admission {
		a := runtimewire.Admission{EndpointID: route}
		a.Binding.RouteID = route
		return a
	}
	current := []runtimewire.Admission{admission("preview_a"), admission("preview_b")}
	delta := control.EndpointUpdate{Removed: []string{"preview_a"}}
	next, _ := mergeEndpointUpdate(current, delta)
	logEndpointRemovals(current, next, delta)
	full := control.EndpointUpdate{Complete: true, Admissions: []runtimewire.Admission{admission("preview_a")}}
	next, _ = mergeEndpointUpdate(current, full)
	logEndpointRemovals(current, next, full)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 ||
		!strings.Contains(lines[0], "msg=endpoint_route_removed route_id=preview_a endpoint_id=preview_a reason=server_removed") ||
		!strings.Contains(lines[1], "msg=endpoint_route_removed route_id=preview_b endpoint_id=preview_b reason=absent_from_full_set") {
		t.Fatalf("removal log = %q", out.String())
	}
	if detachReason(true) != "admission_changed" || detachReason(false) != "not_expected" {
		t.Fatal("detach reasons")
	}
}
