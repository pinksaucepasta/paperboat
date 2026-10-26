package config

import (
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
)

// Edge ingress limits come from one config field, default to edgelimits.Default(), and are validated at startup.
func TestEdgeIngressLimitsDefaultAndValidation(t *testing.T) {
	c := Default()
	if c.EdgeIngressLimits != edgelimits.Default() {
		t.Fatalf("default ingress limits = %+v", c.EdgeIngressLimits)
	}
	c.EdgeIngressLimits.Publication.Connections = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "edge_ingress_limits") {
		t.Fatalf("invalid ingress limits accepted: %v", err)
	}
}
