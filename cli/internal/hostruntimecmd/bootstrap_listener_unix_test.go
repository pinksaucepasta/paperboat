//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
)

func TestRepeatedSetupRetainsOccupiedCarrierDispatchListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := listener.Addr().String()
	root, now := t.TempDir(), time.Now().UTC()
	material := testClientBootstrapMaterial("https://control.example.test", now.Add(time.Hour))
	material.ReuseIdentity = true
	material.EnrollmentID, material.EnrollmentCredential = "", ""
	material.HelperListenAddress = address
	public := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	resume := bootstrap.NewResumeRecord(material.ControlURL, public, "", "studio", strings.Repeat("v", 40), now.Add(time.Hour))
	resume.PairingStarted, resume.Material = true, &material
	carrier := hostinstall.CarrierConfig{RuntimeRoot: root, WorkerAddress: address, Environment: map[string]string{"PAPERBOAT_MACHINE_ID": material.UserMachineID, "PAPERBOAT_CONTROL_URL": material.ControlURL}}
	if err := prepareRetainedBootstrapListener(root, &material, &resume, carrier); err != nil {
		t.Fatal(err)
	}
	saved, err := bootstrap.LoadResume(root, resume.ServerURL, public, "", "studio", now)
	if err != nil {
		t.Fatal(err)
	}
	if material.HelperListenAddress != address || saved.RuntimeListenAddress != address || saved.Material.HelperListenAddress != address {
		t.Fatal("setup moved the worker away from its retained carrier dispatch listener")
	}
	connection, err := net.DialTimeout("tcp4", saved.RuntimeListenAddress, time.Second)
	if err != nil {
		t.Fatal("checkpoint no longer reaches the live account listener", err)
	}
	connection.Close()
	before, err := os.ReadFile(bootstrap.ResumePath(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"runtime_root", "machine", "control_url", "journal_listener"} {
		t.Run(field, func(t *testing.T) {
			candidate := carrier
			candidate.Environment = map[string]string{"PAPERBOAT_MACHINE_ID": material.UserMachineID, "PAPERBOAT_CONTROL_URL": material.ControlURL}
			checkpoint := resume
			switch field {
			case "runtime_root":
				candidate.RuntimeRoot = filepath.Join(root, "foreign")
			case "machine":
				candidate.Environment["PAPERBOAT_MACHINE_ID"] = "machine_foreign"
			case "control_url":
				candidate.Environment["PAPERBOAT_CONTROL_URL"] = "https://foreign.example.test"
			case "journal_listener":
				checkpoint.RuntimeListenAddress = "127.0.0.1:1"
			}
			if err := prepareRetainedBootstrapListener(root, &material, &checkpoint, candidate); !errors.Is(err, bootstrap.ErrResumeBinding) {
				t.Fatalf("different installed binding accepted: %v", err)
			}
			after, err := os.ReadFile(bootstrap.ResumePath(root))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected binding changed protected journal", err)
			}
		})
	}
}
