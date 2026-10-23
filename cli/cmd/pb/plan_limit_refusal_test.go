package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// A send cut at the plan limit by a refused authorization renewal names the limit and the Billing link,
// not "could not finish the command" (Lane O, relay-only containers).
func TestARenewalRefusedAtThePlanLimitNamesIt(t *testing.T) {
	refused := &api.APIError{Status: http.StatusConflict, Code: "bandwidth_quota_exhausted", Message: "This workspace's hosted relay and edge bandwidth is used up. Add bandwidth: https://console.example.test/billing"}
	renewal := fmt.Errorf("file transfer authorization renewal failed: %w", refused)
	err := planLimitRefusal(errors.Join(errors.New("write unix local-api.sock: broken pipe"), renewal))
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	result := classifyCLIJSONError(err)
	if !strings.Contains(message, "Add bandwidth: https://console.example.test/billing") || result.Code != "bandwidth_quota_exhausted" {
		t.Fatalf("message=%q code=%q", message, result.Code)
	}
	if other := errors.New("dial"); planLimitRefusal(other) != other {
		t.Fatal("an unrelated failure changed")
	}
}
