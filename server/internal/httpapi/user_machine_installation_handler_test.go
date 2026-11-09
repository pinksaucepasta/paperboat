package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordingInstallationConsumer struct {
	verifier          string
	publicIdentityKey string
	runtimeEnrolled   bool
}

func (c *recordingInstallationConsumer) ConsumeInstallationForIdentityState(_ context.Context, verifier, publicIdentityKey string, runtimeEnrolled bool) (json.RawMessage, error) {
	c.verifier = verifier
	c.publicIdentityKey = publicIdentityKey
	c.runtimeEnrolled = runtimeEnrolled
	return json.RawMessage(`{"machine_id":"machine-test"}`), nil
}

func TestUserMachineInstallationConsumeForwardsRuntimeEnrollmentState(t *testing.T) {
	consumer := &recordingInstallationConsumer{}
	request := httptest.NewRequest(http.MethodPost, "/v1/machines/pairings/installation", strings.NewReader(`{"verifier":"verifier-test","public_identity_key":"identity-test","runtime_enrolled":true}`))
	response := httptest.NewRecorder()

	userMachineInstallationConsume(consumer).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if consumer.verifier != "verifier-test" || consumer.publicIdentityKey != "identity-test" || !consumer.runtimeEnrolled {
		t.Fatalf("forwarded request = verifier %q identity %q runtime_enrolled %t", consumer.verifier, consumer.publicIdentityKey, consumer.runtimeEnrolled)
	}
}

// The verifier secret is read by one strict decoder everywhere: unknown fields and trailing data are refused.
func TestUserMachineInstallationConsumeRefusesUnknownFieldsAndTrailingData(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field": `{"verifier":"v","public_identity_key":"k","runtime_enrolled":true,"extra":1}`,
		"trailing data": `{"verifier":"v","public_identity_key":"k"}{"verifier":"w"}`,
	} {
		consumer := &recordingInstallationConsumer{}
		response := httptest.NewRecorder()
		userMachineInstallationConsume(consumer).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/machines/pairings/installation", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "validation_failed") || consumer.verifier != "" {
			t.Fatalf("%s: status %d body %s verifier %q", name, response.Code, response.Body.String(), consumer.verifier)
		}
	}
}
