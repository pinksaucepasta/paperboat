package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

func TestTerminalCapacityHTTPErrorExplainsRecovery(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/machines/machine_1/terminal-sessions", nil)
	if !userMachineTerminalSessionError(w, r, &usermachines.TerminalSessionCapacityError{LiveSessions: 20, Limit: 20}) {
		t.Fatal("capacity error was not handled")
	}
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"terminal_session_limit_reached"`) || !strings.Contains(w.Body.String(), "Close one to start another") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
