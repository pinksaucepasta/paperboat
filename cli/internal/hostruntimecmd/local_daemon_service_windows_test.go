//go:build windows

package hostruntimecmd

import (
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"reflect"
	"testing"
)

func TestLocalDaemonServiceUsesCanonicalDaemonEntry(t *testing.T) {
	install := hostinstall.WindowsRuntimeConfig{ControlURL: "https://api.paperboat.test", OwnerSID: "S-1-5-21-1-2-3-1001", Instance: "u0123456789abcdef01234567"}
	config := localDaemonServiceConfig(install, `C:\Program Files\Paperboat\bin\pb.exe`)
	if want := []string{"daemon", "--server", install.ControlURL}; !reflect.DeepEqual(config.Arguments, want) {
		t.Fatalf("service child invocation=%q; want canonical daemon entry %q", config.Arguments, want)
	}
	if config.EnrolledSID != install.OwnerSID || config.Name != "PaperboatLocalDaemon-"+install.Instance || config.LaunchFailure == nil {
		t.Fatal("lost enrolled service ownership or launch diagnostics")
	}
}
