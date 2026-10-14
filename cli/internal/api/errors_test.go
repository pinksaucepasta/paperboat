package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestStructuredAuthFailurePreservesStatusReferenceAndSafeRecovery(t *testing.T) {
	reference := "support_01234567-89ab-4def-8123-456789abcdef"
	for _, kind := range []string{"authenticated", "device", "tunnel"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get(supportref.Header) != reference {
					t.Error("request reference changed")
				}
				if calls == 1 {
					w.Header().Set("Request-Id", "credential_secret")
					w.WriteHeader(401)
					io.WriteString(w, `{"error":{"code":"unauthenticated","message":"private_token secret@example.invalid\npassword=secret","details":{"private":"secret"}}}`)
					return
				}
				io.WriteString(w, `{"data":{"ok":true}}`)
			}))
			defer server.Close()
			c := New(server.URL, config.Credential{}, server.Client())
			ctx := supportref.WithContext(context.Background(), reference)
			call := func(out any) error {
				switch kind {
				case "authenticated":
					return c.do(ctx, http.MethodGet, "/v1/test", nil, out)
				case "device":
					return publicCall(ctx, server.URL, "/v1/auth/test", nil, "", out, server.Client())
				default:
					return c.doTunnelRequest(ctx, http.MethodGet, "/v1/tunnels/test", nil, out, nil, nil)
				}
			}
			var out struct {
				Ok bool `json:"ok"`
			}
			err := call(&out)
			var apiErr *APIError
			if !errors.Is(err, ErrUnauthenticated) || !errors.As(err, &apiErr) || apiErr.DiagnosticStatus() != 401 || apiErr.SupportReference != reference {
				t.Fatalf("auth metadata lost: %T", err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(apiErr.Message, "secret") || !strings.Contains(apiErr.Message, "login") {
				t.Fatal("private response text exposed or owned recovery absent")
			}
			if err := call(&out); err != nil || !out.Ok || calls != 2 {
				t.Fatalf("recovery=%v calls=%d", err, calls)
			}
		})
	}
}

func TestProtocolDecodeErrorPreservesCauseWithoutDisplayingPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":{"private_credential_name":"private-value"}}`)
	}))
	defer server.Close()
	var out struct {
		ID string `json:"id"`
	}
	err := New(server.URL, config.Credential{}, server.Client()).doStrict(context.Background(), http.MethodGet, "/v1/test", nil, &out)
	var decodeErr *ResponseDecodeError
	if !errors.As(err, &decodeErr) || errors.Unwrap(decodeErr) == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("decode error unsafe/lost cause: %T", err)
	}
	// Typed decoder errors retain normal errors.As behavior as well.
	cause := &json.SyntaxError{Offset: 2}
	if !errors.As(&ResponseDecodeError{Err: cause}, new(*json.SyntaxError)) {
		t.Fatal("typed decode cause lost")
	}
}

func TestPublicMessagesRejectArbitraryVersionAndRequestMetadata(t *testing.T) {
	for _, err := range []error{
		&APIError{Status: 409, Code: "private_code", Message: "private_secret", RequestID: "private_request"},
		&ErrIncompatibleVersion{Required: "private_protocol", Message: "private_upgrade"},
	} {
		if strings.Contains(err.Error(), "private") {
			t.Fatal("arbitrary response text became public error")
		}
	}
	if !strings.Contains((&ErrIncompatibleVersion{Required: "2"}).Error(), "protocol 2") {
		t.Fatal("valid required protocol lost")
	}
}

func TestPublicAPICodePreservesKnownMeaningAndDropsUnknownPayload(t *testing.T) {
	for _, code := range []string{"machine_offline", "machine_revoked", "workspace_unavailable", "team_subscription_required", "favorite_limit_reached"} {
		if got := (&APIError{Code: code}).PublicCode(); got != code {
			t.Fatalf("known code lost: %s", code)
		}
	}
	for _, code := range []string{"private_secret", "token_safe_identifier", "unknown", ""} {
		if got := (&APIError{Code: code}).PublicCode(); got != "api_error" {
			t.Fatalf("unknown code exported: %s", got)
		}
	}
}

func TestOperationErrorOwnedProsePreservesRecoveryFields(t *testing.T) {
	value := &PreviewTunnelAPIError{Code: "machine_offline", Message: "PRIVATE_VALUE", RepairAction: "inspect_operation", Outcome: "unchanged", Retryable: true, RequestID: "request_1", CorrelationID: "correlation_1"}
	projectOperationError(value)
	if value.Message != publicAPIMessages["machine_offline"] || value.RepairAction != "inspect_operation" || !value.Retryable || value.Outcome != "unchanged" || value.Code != "machine_offline" {
		t.Fatalf("projection=%#v", value)
	}
}
