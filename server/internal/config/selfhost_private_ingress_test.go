package config

import (
	"strings"
	"testing"
)

// The staging switch that lets self-hosted claims reach private addresses is refused in production and
// accepted elsewhere.
func TestSelfhostPrivateIngressIsRefusedInProduction(t *testing.T) {
	for _, env := range []Environment{EnvironmentProduction, EnvironmentDevelopment} {
		c := Default()
		c.Environment = env
		c.SelfhostAllowPrivateIngress = true
		err := c.Validate()
		refused := err != nil && strings.Contains(err.Error(), "selfhost_allow_private_ingress")
		if refused != (env == EnvironmentProduction) {
			t.Fatalf("%s: private ingress refused=%v (%v)", env, refused, err)
		}
	}
}
