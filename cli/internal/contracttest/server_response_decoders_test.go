package contracttest

import (
	"os"
	"strings"
	"testing"
)

// Documents received from the control plane are decoded tolerantly: none of these server-response
// decoders may reject unknown fields again (local state files and local IPC stay strict).
func TestServerResponseDecodersDoNotRejectUnknownFields(t *testing.T) {
	for _, path := range []string{
		"../hostruntime/machinecontrol/source.go",
		"../hostruntime/enrollment/client.go",
		"../hostruntime/preview/attachment_validate.go",
		"../hostruntime/preview/readiness_observer.go",
		"../hostruntime/preview/accessor_discovery.go",
		"../hostruntime/preview/private_access.go",
		"../hostruntime/tunnelenrollment/client.go",
		"../hostruntime/tunnelenrollment/bootstrap_descriptor.go",
		"../hostruntime/peeridentity/enroll.go",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "DisallowUnknownFields") {
			t.Errorf("%s rejects unknown fields in a server response; use wirejson so a newer server never breaks this client", path)
		}
	}
}
