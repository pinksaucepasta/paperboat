package config

import "testing"

// The deployment a 2026.11.16.2 pbh exported (testdata written by that release's exportRuntime, with its
// browser_login_origin) loads here: a field this build dropped is ignored, not a reason to refuse to start.
func TestLoadsDeploymentWrittenByPreviousRelease(t *testing.T) {
	deployment, err := LoadDeployment("testdata/deployment-2026.11.16.2.json")
	if err != nil || !deployment.BrowserAccessEnabled || deployment.ConnectorAdvertiseHost != "edge.example.test" {
		t.Fatalf("previous release's deployment = %+v, %v", deployment, err)
	}
}
