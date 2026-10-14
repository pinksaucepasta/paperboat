package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestTunnelMalformedErrorResponseRetainsProtocolCauseAndReference(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			reference := supportref.New()
			client := tunnelTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(supportref.Header, reference)
				w.WriteHeader(status)
				_, _ = w.Write([]byte("PRIVATE_PROVIDER_PAYLOAD"))
			})
			_, err := client.TunnelStatusV1(t.Context(), "tun_1")
			var response *APIError
			var decode *ResponseDecodeError
			var syntax *json.SyntaxError
			if !errors.As(err, &response) || response.Status != status || response.SupportReference != reference || response.Code != "invalid_server_response" ||
				!errors.As(err, &decode) || !errors.As(err, &syntax) {
				t.Fatalf("malformed error response lost status, reference or original protocol cause: %v", err)
			}
			if strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatal("public error exposed provider body")
			}
		})
	}
}
