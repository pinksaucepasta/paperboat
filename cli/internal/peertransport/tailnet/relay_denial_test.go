package tailnet

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type deniedNetworkAPI struct {
	token  string
	result *api.PeerNetworkConfigurationResult
}

func (deniedNetworkAPI) RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	return api.PeerNetworkRegistrationResult{}, nil
}

func (d deniedNetworkAPI) PeerNetworkConfiguration(context.Context, api.PeerNetworkConfigurationRequest) (api.PeerNetworkConfigurationResult, error) {
	result := *d.result
	result.Configuration = d.token
	return result, nil
}

// F13-03: a refresh that withheld hosted relays records why, without dropping the admitted
// direct peers, and clears the reason when relays return.
func TestRefreshRecordsRelayDenialAndClearsItOnRecovery(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	result := &api.PeerNetworkConfigurationResult{RelayDenied: "bandwidth_quota_exhausted", BillingURL: "https://console.example.test/billing"}
	client := deniedNetworkAPI{result: result}
	cfg.Generation++
	client.token = networkToken(t, signer, cfg)
	if err := a.Refresh(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	denial := a.RelayDenial()
	if denial == nil || denial.Code != "bandwidth_quota_exhausted" {
		t.Fatalf("denial not recorded: %+v", denial)
	}
	if denial.Details["billing_url"] != "https://console.example.test/billing" {
		t.Fatalf("billing url lost: %v", denial.Details)
	}
	if !a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("the denial dropped direct peer authority")
	}
	result.RelayDenied, result.BillingURL = "", ""
	cfg.Generation++
	client.token = networkToken(t, signer, cfg)
	if err := a.Refresh(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	if a.RelayDenial() != nil {
		t.Fatal("denial survived relay recovery")
	}
}
