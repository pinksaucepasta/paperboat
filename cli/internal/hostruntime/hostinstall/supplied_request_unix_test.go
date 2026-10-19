//go:build darwin || linux

package hostinstall

import "testing"

// A fresh machine guard learns the local access domain from the server the install
// request names, so an unenrolled supplied install must not drop it.
func TestUnboundSuppliedRequestKeepsTheControlURL(t *testing.T) {
	request := unboundSuppliedRequest(Request{Schema: SchemaV1, Platform: "linux", User: "tester", UID: 1000, GID: 1000, Group: "tester", Home: "/home/tester", Executable: "/home/tester/pb", ControlURL: "https://api.example.test", Force: true})
	if request.ControlURL != "https://api.example.test" {
		t.Fatalf("control URL = %q; a fresh machine guard could not read its local access domain", request.ControlURL)
	}
	if !request.EnrollmentPending || request.UID != 1000 || request.Executable != "/home/tester/pb" || request.Force {
		t.Fatalf("unbound request changed shape: %+v", request)
	}
}
