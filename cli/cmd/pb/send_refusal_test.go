package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// F6-05: sending to an offline computer is refused before anything is sent.
func TestSendRefusalsReportNothingChanged(t *testing.T) {
	cases := map[string]struct {
		err        error
		code, text string
	}{
		"offline": {&api.APIError{Status: http.StatusConflict, Code: "machine_offline", Message: "The destination machine is offline."}, "machine_offline", "offline"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := classifyCLIJSONError(tc.err)
			if result.Code != tc.code || result.StateChanged != false || !strings.Contains(strings.ToLower(result.Message), tc.text) {
				t.Fatalf("code=%q state_changed=%v message=%q", result.Code, result.StateChanged, result.Message)
			}
		})
	}
}
