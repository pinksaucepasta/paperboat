//go:build darwin || linux

package hostruntimecmd

import (
	"os/user"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

// A fresh host has no recorded local access domain, so the privileged install
// reads it from the server named by the request: the request must carry it.
func TestSuppliedInstallRequestCarriesTheControlURL(t *testing.T) {
	request := supplyInstallRequest(&user.User{Username: "tester", HomeDir: "/home/tester"}, 1000, 1000, "tester", "/home/tester/pb", installsource.Source{}, "", false, true, "https://api.example.test")
	if request.ControlURL != "https://api.example.test" {
		t.Fatalf("install request ControlURL = %q; a fresh machine guard could not discover its local access domain", request.ControlURL)
	}
	if request.UID != 1000 || !request.SuppliedUpdate || !request.EnrollmentPending || request.Home != "/home/tester" {
		t.Fatalf("install request lost its identity fields: %+v", request)
	}
}
