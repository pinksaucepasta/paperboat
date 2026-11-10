//go:build windows

package hostruntimecmd

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
)

// Clean Windows Sandbox, paired official install: `pb update --from official.pb-update` failed with "elevated
// Windows operation failed" (cause: install_source: source: invalid privileged installation request). The CLI sends
// a bundle request without an executable or a source; the installer validated it before resolving the bundle.
func TestABundleRequestIsCheckedOnlyAfterItsBundleIsResolved(t *testing.T) {
	bundle := hostinstall.Request{Schema: hostinstall.SchemaV1, Platform: "windows", UpdateBundle: `C:\Users\pb\official.pb-update`, SuppliedUpdate: true}
	if err := validateSuppliedWindowsRequest(bundle); err != nil {
		t.Fatalf("a bundle request was refused before its bundle was resolved: %v", err)
	}
	// Resolved (or never a bundle): the same request without an identity is refused.
	bundle.UpdateBundle = ""
	if err := validateSuppliedWindowsRequest(bundle); err == nil {
		t.Fatal("a request without an executable identity was accepted")
	}
}
