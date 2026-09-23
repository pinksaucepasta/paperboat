package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestStructuredAPIErrorIncludesSeparateSupportReference(t *testing.T) {
	value := classifyCLIJSONError(&api.APIError{
		Status: http.StatusBadGateway, Code: "upstream_failed",
		RequestID: "req_123", SupportReference: "pb-0123456789abcdef0123456789abcdef",
	})
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["request_id"]; exists || got["support_reference"] != "pb-0123456789abcdef0123456789abcdef" {
		t.Fatalf("structured error = %s", encoded)
	}
}
