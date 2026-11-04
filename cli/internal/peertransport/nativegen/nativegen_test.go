package nativegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
)

type machineAuth struct{}

func (machineAuth) Token(context.Context) (string, error) { return "machine-token", nil }
func (machineAuth) Proof(_ context.Context, operation, method, path string, body []byte) ([]byte, error) {
	return []byte(strings.Join([]string{operation, method, path, string(body)}, "|")), nil
}

// Report sends the offer's exact bytes to the machine-proof carrier route (the proof covers the body and its
// operation id); a zero offer withdraws with an empty offer; without Use it fails instead of dropping the report.
func TestReportSendsTheSignedOfferWithMachineProof(t *testing.T) {
	control.Store(nil)
	if err := Report(t.Context(), native.CarrierOffer{}); !errors.Is(err, ErrNoControl) {
		t.Fatalf("without Use: %v", err)
	}
	var got []api.CarrierOfferReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var report api.CarrierOfferReport
		proof, _ := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if r.Method != http.MethodPost || r.URL.Path != "/v1/machine-peer-network/carrier" || r.Header.Get("Authorization") != "Bearer machine-token" || json.Unmarshal(body, &report) != nil || string(proof) != report.OperationID+"|POST|/v1/machine-peer-network/carrier|"+string(body) {
			t.Errorf("request %s %s proof=%q body=%s", r.Method, r.URL.Path, proof, body)
		}
		got = append(got, report)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{}}`)
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{}, server.Client())
	client.SetMachineAuth(machineAuth{})
	Use(client)
	offer := native.CarrierOffer{Claims: native.CarrierClaims{Kind: "paperboat.terminal-carrier/v1", MachineID: "machine_1", AllApplications: true}, Signature: []byte("signature")}
	want, _ := offer.MarshalBinary()
	if err := Report(t.Context(), offer); err != nil {
		t.Fatal(err)
	}
	if err := Report(t.Context(), native.CarrierOffer{}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got[0].Offer) != string(want) || len(got[1].Offer) != 0 || got[0].OperationID == got[1].OperationID {
		t.Fatalf("reports %+v", got)
	}
}
