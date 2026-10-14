//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// The privileged status response is the native startup fence's commit proof.
// Readiness, a matching version, and an old image cannot substitute for the
// exact committed executing image; a staging handoff can outlive native commit.
func TestNativeStartupAdmissionRequiresExactPrivilegedCommitProof(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires a privileged native updater peer")
	}
	expected := installsource.Source{Version: "2026.10.14.1", SHA256: strings.Repeat("a", 64), Length: 42}
	base := updated.ControlResponse{Schema: updated.ControlProtocolV1, Status: "ok", Pending: true, Transaction: workerupdate.TransactionState{Schema: workerupdate.TransactionSchemaV1, ActiveVersion: expected.Version, ActiveDigest: expected.SHA256, ActiveLength: expected.Length}}
	for _, scenario := range []string{"readiness", "old-version", "wrong-image", "wrong-length", "wrong-schema", "committed"} {
		t.Run(scenario, func(t *testing.T) {
			response := base
			switch scenario {
			case "readiness":
				response.Transaction.ActiveVersion, response.Transaction.ActiveDigest, response.Transaction.ActiveLength = "", "", 0
			case "old-version":
				response.Transaction.ActiveVersion = "2026.10.13.1"
			case "wrong-image":
				response.Transaction.ActiveDigest = strings.Repeat("b", 64)
			case "wrong-length":
				response.Transaction.ActiveLength++
			case "wrong-schema":
				response.Transaction.Schema = "unknown"
			}
			path := filepath.Join(t.TempDir(), "u.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(time.Second))
				var request updated.ControlRequest
				if json.NewDecoder(connection).Decode(&request) == nil && request.Operation == "status" {
					_ = json.NewEncoder(connection).Encode(response)
				}
			}()
			client, err := updated.NewClient(path, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			controller, err := hostdproto.NewController(hostdproto.ControllerConfig{APIMin: 1, APIMax: 1, MaintenancePending: true, VerifyMaintenanceCommitted: committedSourceVerifier(expected, client)})
			if err != nil {
				t.Fatal(err)
			}
			welcome, err := controller.Negotiate(hostdproto.Hello{WorkerID: "candidate", Version: expected.Version, APIMin: 1, APIMax: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err = controller.MarkReady(hostdproto.Ready{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}); err != nil {
				t.Fatal(err)
			}
			if _, err = controller.Activate(hostdproto.Activate{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}); err != nil {
				t.Fatal(err)
			}
			err = controller.CompleteMaintenance(context.Background())
			<-done
			if scenario != "committed" {
				if !errors.Is(err, hostdproto.ErrMaintenanceBusy) {
					t.Fatal("noncommitted source accepted", err)
				}
				if _, err = controller.AcquireActive(welcome.WorkerID, welcome.Epoch); !errors.Is(err, hostdproto.ErrMaintenanceBusy) {
					t.Fatal("mutation crossed pending native commit", err)
				}
				return
			}
			if err != nil {
				t.Fatal("exact committed source did not release admission", err)
			}
			release, err := controller.AcquireActive(welcome.WorkerID, welcome.Epoch)
			if err != nil {
				t.Fatal(err)
			}
			release()
		})
	}
}
