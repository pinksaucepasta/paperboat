package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/environment"
)

func TestEnvironmentUnconfiguredErrorIncludesOnlyBootstrapCoordinates(t *testing.T) {
	err := &environment.EnvironmentUnconfiguredError{
		MachineID:              "machine_123",
		InstallationGeneration: 4,
		HostKeyGeneration:      2,
		HostPublic:             "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA",
	}
	result := httptest.NewRecorder()
	writeEnvironmentError(result, httptest.NewRequest(http.MethodPost, "/v1/environment/hosts/machine_123/key", nil), err)
	if result.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", result.Code, result.Body.String())
	}
	var response ErrorResponse
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "environment_unconfigured" {
		t.Fatalf("code=%q body=%s", response.Error.Code, result.Body.String())
	}
	want := map[string]any{
		"machine_id":                "machine_123",
		"installation_generation":   float64(4),
		"host_key_generation":       float64(2),
		"host_public":               "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA",
		"account_config_generation": float64(0),
	}
	if len(response.Error.Details) != len(want) {
		t.Fatalf("details=%v", response.Error.Details)
	}
	for key, value := range want {
		if response.Error.Details[key] != value {
			t.Errorf("details[%q]=%v want %v", key, response.Error.Details[key], value)
		}
	}
	if _, exists := response.Error.Details["host_private"]; exists {
		t.Fatal("private host key leaked into error details")
	}
}
