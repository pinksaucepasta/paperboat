package httpapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
)

func TestTerminalSessionErrorStaysQuietWhenTheCallerWentAway(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/machines/m/terminal-sessions", nil)
	w := httptest.NewRecorder()
	if !userMachineTerminalSessionError(w, r, fmt.Errorf("list terminal sessions: %w", context.Canceled)) {
		t.Fatal("a canceled request was not treated as handled")
	}
	if w.Body.Len() != 0 {
		t.Fatalf("a reply was written to a caller that left: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	userMachineTerminalSessionError(w, r, fmt.Errorf("storage failed"))
	if w.Code != 500 {
		t.Fatalf("a real failure answered %d, want 500", w.Code)
	}
}
