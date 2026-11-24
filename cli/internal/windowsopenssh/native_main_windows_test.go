//go:build windows

package windowsopenssh

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hostservice "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
)

// The native tests install the Paperboat service exactly as production does (hostinstall.windowsOpenSSHConfig):
// per-instance name, the service SID, a state root under ProgramData, a port derived from the instance. Production's
// service entry point (`pb daemon __windows-sshd-service --instance <id>`) reads an enrolled install; here the test
// binary is the service executable and answers that command line from nativeFixture, so no enrollment is needed.
type nativeFixture struct{ OwnerSID, StateRoot string }

func nativeFixturePath(instance string) string {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, "Paperboat", "native-ssh-test", instance+".json")
}

func nativeConfig(t *testing.T) Config {
	t.Helper()
	config := DefaultConfig(nil)
	instance, err := hostservice.WindowsUserInstance(config.OwnerSID)
	if err != nil {
		t.Fatal(err)
	}
	config.ServiceName = ServiceName + "-" + instance
	config.ServiceSID = ServiceSID(config.ServiceName)
	programData := filepath.Dir(filepath.Dir(nativeFixturePath(instance)))
	config.StateRoot = filepath.Join(programData, "Paperboat", "users", instance, "ssh")
	if digest, err := hex.DecodeString(strings.TrimPrefix(instance, "u")); err == nil && len(digest) >= 2 {
		config.Port = uint16(40000 + (uint32(digest[0])<<8|uint32(digest[1]))%20000)
	}
	if config.ServiceExecutable, err = os.Executable(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(nativeFixture{OwnerSID: config.OwnerSID, StateRoot: config.StateRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(nativeFixturePath(instance)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativeFixturePath(instance), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestMain(m *testing.M) {
	if len(os.Args) == 5 && os.Args[1] == "daemon" && os.Args[2] == "__windows-sshd-service" && os.Args[3] == "--instance" {
		os.Exit(runNativeServiceChild(os.Args[4]))
	}
	os.Exit(m.Run())
}

func runNativeServiceChild(instance string) int {
	data, err := os.ReadFile(nativeFixturePath(instance))
	var fixture nativeFixture
	if err != nil || json.Unmarshal(data, &fixture) != nil {
		return 1
	}
	config := DefaultConfig(nil)
	if RunServiceHost(ServiceName+"-"+instance, filepath.Join(config.InstallRoot, "sshd.exe"), filepath.Join(fixture.StateRoot, "sshd_config"), fixture.OwnerSID) != nil {
		return 1
	}
	return 0
}
