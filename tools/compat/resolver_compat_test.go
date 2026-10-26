// compat:target cli/internal/resolver
package resolver

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

// TestCompatFileTransferDescriptor: the file transfer descriptor an undeclared (released) sender
// receives passes this release's descriptor validation.
func TestCompatFileTransferDescriptor(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(os.Getenv("COMPAT_ARTIFACTS_DIR"), "file_transfer_descriptor.json"))
	if err != nil {
		t.Fatal(err)
	}
	var transfer api.FileTransfer
	if err := json.Unmarshal(raw, &transfer); err != nil {
		t.Fatal(err)
	}
	route, err := url.Parse(transfer.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	r := &APIResolver{cfg: &config.Config{}}
	if err := r.validateFileTransfer(&transfer, route, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("descriptor refused by this release: %v", err)
	}
}
