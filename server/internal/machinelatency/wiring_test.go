package machinelatency

import (
	"os"
	"strings"
	"testing"
)

// The wiring below was lost twice to concurrent edits of shared files (router.go, app.go and the
// attachment service), so the deployed server accepted reports and quietly dropped them. A real
// PostgreSQL attachment test cannot run in every gate; this one fails whenever the calls are gone.
func TestLatencyIsWiredIntoTheAttachmentAndTheApplication(t *testing.T) {
	for file, needle := range map[string]string{
		"../runtimeattachment/service.go": "machinelatency.SaveEdgesTx(",
		"../httpapi/router.go":            "network-latency",
		"../app/app.go":                   "MachineLatency:",
	} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(source), needle) {
			t.Errorf("%s no longer contains %q: machine latency would be dropped", file, needle)
		}
	}
}
