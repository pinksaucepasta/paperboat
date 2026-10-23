package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/managedssh"
)

func TestHostKeyInUseIsATypedConflictNamingOnlyAVisibleMachine(t *testing.T) {
	for _, tc := range []struct {
		name, want string
	}{{"office-vm", `"machine_name":"office-vm"`}, {"", ""}} {
		rec := httptest.NewRecorder()
		managedSSHError(rec, httptest.NewRequest(http.MethodPut, "/v1/machines/machine_x/ssh-host-keys", nil), &managedssh.HostKeyInUseError{MachineName: tc.name})
		body := rec.Body.String()
		if rec.Code != http.StatusConflict || !strings.Contains(body, `"code":"ssh_host_key_in_use"`) || !strings.Contains(body, `"state_changed":false`) {
			t.Fatalf("status=%d body=%s", rec.Code, body)
		}
		if tc.want != "" && !strings.Contains(body, tc.want) || tc.want == "" && strings.Contains(body, "machine_name") {
			t.Fatalf("machine name exposure wrong for %q: %s", tc.name, body)
		}
	}
}
