package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

func TestTerminalDeleteLiveHistoryExplainsRecovery(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/v1/machines/machine_1/terminal-sessions/session_1", nil)
	if !userMachineTerminalSessionError(w, r, usermachines.ErrTerminalSessionNotClosed) {
		t.Fatal("not-closed error was not handled")
	}
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"terminal_session_not_closed"`) || !strings.Contains(w.Body.String(), "Close the terminal and wait") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
