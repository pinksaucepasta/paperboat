package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
)

// A new public raw-TCP tunnel past the plan's limit is a typed 409 that names the limit and both ways out.
func TestPublicTCPLimitIsATyped409(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/tunnels", nil)
	err := fmt.Errorf("create tunnel: %w", &planlimits.LimitError{Kind: planlimits.PublicTCP, Plan: planlimits.Free, Limit: 1, Used: 1})
	if !writePreviewTunnelLimitReached(recorder, request, err) {
		t.Fatal("the limit was not handled")
	}
	var body struct {
		Error struct{ Code, Message string }
	}
	if recorder.Code != http.StatusConflict || json.Unmarshal(recorder.Body.Bytes(), &body) != nil {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if body.Error.Code != "public_tcp_limit_reached" || !strings.Contains(body.Error.Message, "limit of 1 public TCP tunnels") || !strings.Contains(body.Error.Message, "upgrade your plan") || !strings.Contains(body.Error.Message, "own tunnel edge") {
		t.Fatalf("error=%+v", body.Error)
	}
}

// Free includes no public TCP tunnel (user decision 2026-11-28): the refusal says Pro is needed and names private access.
func TestPublicTCPOnFreeNeedsPro(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/tunnels", nil)
	err := &planlimits.LimitError{Kind: planlimits.PublicTCP, Plan: planlimits.Free, Limit: 0, Used: 0}
	if !writePreviewTunnelLimitReached(recorder, request, err) {
		t.Fatal("the limit was not handled")
	}
	var body struct {
		Error struct{ Code, Message string }
	}
	if recorder.Code != http.StatusConflict || json.Unmarshal(recorder.Body.Bytes(), &body) != nil {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if body.Error.Code != "public_tcp_limit_reached" || !strings.HasPrefix(body.Error.Message, "Public TCP tunnels need Pro; use private access through pb") || strings.Contains(body.Error.Message, "limit of 0") {
		t.Fatalf("error=%+v", body.Error)
	}
}
