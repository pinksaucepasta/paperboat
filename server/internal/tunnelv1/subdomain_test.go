package tunnelv1

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	tunnelshared "github.com/pinksaucepasta/paperboat/shared/tunnelv1"
)

func TestValidateSubdomain(t *testing.T) {
	reserved := map[string]struct{}{"www": {}, "admin": {}}
	for label, want := range map[string]error{
		"helloworld":            nil,
		"a1b":                   nil,
		"my-app-2":              nil,
		strings.Repeat("a", 63): nil,
		"ab":                    ErrSubdomainInvalid,
		strings.Repeat("a", 64): ErrSubdomainInvalid,
		"Hello":                 ErrSubdomainInvalid,
		"-abc":                  ErrSubdomainInvalid,
		"abc-":                  ErrSubdomainInvalid,
		"a.b":                   ErrSubdomainInvalid,
		"a_b":                   ErrSubdomainInvalid,
		"héllo":                 ErrSubdomainInvalid,
		"":                      ErrSubdomainInvalid,
		"12345":                 ErrSubdomainInvalid,
		"xn--bcher-kva":         ErrSubdomainInvalid,
		"www":                   ErrSubdomainReserved,
		"admin":                 ErrSubdomainReserved,
	} {
		if err := validateSubdomain(label, reserved); !errors.Is(err, want) || (want == nil && err != nil) {
			t.Errorf("validateSubdomain(%q) = %v, want %v", label, err, want)
		}
	}
	if _, err := reservedSet([]string{"Bad"}); err == nil {
		t.Fatal("an invalid reserved label was accepted")
	}
}

func TestCheckSubdomainAnswersEveryReason(t *testing.T) {
	repository := &fakeTunnelRepository{tunnel: dbsqlc.Tunnel{ID: "tun_1", AccountID: "acct_1", StableEndpoint: "https://eager-purple-lantern-7514.tunnels.example.test"}}
	service := testService(t, repository, sequentialID())
	for _, tc := range []struct {
		label, reason string
		taken         bool
		available     bool
	}{
		{label: "helloworld", available: true},
		{label: "helloworld", reason: "taken", taken: true},
		{label: "admin", reason: "reserved"},
		{label: "Hello", reason: "invalid"},
		{label: "eager-purple-lantern-7514", reason: "current"},
	} {
		repository.addressTaken = tc.taken
		got, err := service.CheckSubdomain(context.Background(), testRequest(false), "tun_1", tc.label)
		if err != nil || got.Available != tc.available || got.Reason != tc.reason {
			t.Errorf("CheckSubdomain(%q) = %+v, %v; want available=%v reason=%q", tc.label, got, err, tc.available, tc.reason)
		}
		if tc.available && repository.checkedLabel != "helloworld" {
			t.Errorf("checked label %q", repository.checkedLabel)
		}
	}
}

func TestPatchTunnelRejectsInvalidAndReservedSubdomainBeforeTheRepository(t *testing.T) {
	repository := &fakeTunnelRepository{}
	service := testService(t, repository, sequentialID())
	for label, want := range map[string]error{"Bad Name": ErrSubdomainInvalid, "www": ErrSubdomainReserved} {
		label := label
		_, err := service.PatchTunnel(context.Background(), testRequest(false), "tun_1", PatchTunnelRequest{
			Subdomain: &label, MutationInput: MutationInput{ExpectedGeneration: 1, IdempotencyKey: "op_patch_1", RequestHash: testHash()},
		})
		if !errors.Is(err, want) || repository.patch.TunnelID != "" {
			t.Fatalf("%q: err=%v patch reached repository=%+v", label, err, repository.patch)
		}
	}
}

// A self-hosted workspace's tunnel has no managed address: the answer says where its address is set instead.
func TestCheckSubdomainSaysASelfHostedTunnelHasNoManagedAddress(t *testing.T) {
	repository := &fakeTunnelRepository{tunnel: dbsqlc.Tunnel{ID: "tun_1", AccountID: "acct_1", StableEndpoint: "https://eager.tunnels.example.test"}, addressSelfHost: true}
	service := testService(t, repository, sequentialID())
	got, err := service.CheckSubdomain(context.Background(), testRequest(false), "tun_1", "helloworld")
	if err != nil || got.Available || got.Reason != "unavailable" || !strings.Contains(got.Message, "pb tunnel domain add") {
		t.Fatalf("CheckSubdomain = %+v, %v", got, err)
	}
}

// The tunnel views carry the address change in progress (or the failed one) the repository reports.
func TestGetTunnelCarriesTheAddressChange(t *testing.T) {
	change := tunnelshared.AddressChange{State: "moving", Hostname: "helloworld.tunnels.example.test"}
	repository := &fakeTunnelRepository{tunnel: dbsqlc.Tunnel{ID: "tun_1", AccountID: "acct_1", StableEndpoint: "https://eager.tunnels.example.test"}, addressChanges: map[string]tunnelshared.AddressChange{"tun_1": change}}
	service := testService(t, repository, sequentialID())
	got, err := service.GetTunnel(context.Background(), testRequest(false), "tun_1")
	if err != nil || got.AddressChange == nil || *got.AddressChange != change {
		t.Fatalf("GetTunnel address change = %+v, %v", got.AddressChange, err)
	}
	repository.addressChanges = nil
	if got, err := service.GetTunnel(context.Background(), testRequest(false), "tun_1"); err != nil || got.AddressChange != nil {
		t.Fatalf("a tunnel with no change = %+v, %v", got.AddressChange, err)
	}
}

func TestCheckSubdomainErrorsAreNotAnswers(t *testing.T) {
	repository := &fakeTunnelRepository{tunnel: dbsqlc.Tunnel{ID: "tun_1", AccountID: "acct_1", StableEndpoint: "https://eager.tunnels.example.test"}}
	service := testService(t, repository, sequentialID())
	repository.managementErr = ErrNotFound
	if _, err := service.CheckSubdomain(context.Background(), testRequest(false), "prv_1", "helloworld"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a preview or unknown ID = %v, want ErrNotFound", err)
	}
	repository.managementErr = nil
	cause := errors.New("database is down")
	repository.addressErr = cause
	if _, err := service.CheckSubdomain(context.Background(), testRequest(false), "tun_1", "helloworld"); !errors.Is(err, cause) {
		t.Fatalf("a failed lookup = %v, want the cause", err)
	}
	request := testRequest(false)
	request.Actor.Scopes = []string{"tunnels:read"}
	if _, err := service.CheckSubdomain(context.Background(), request, "tun_1", "helloworld"); err == nil {
		t.Fatal("a read-only actor could probe names")
	}
}

func TestSharedActorsCannotChangeAnAddress(t *testing.T) {
	repository := &fakeTunnelRepository{tunnel: dbsqlc.Tunnel{ID: "tun_1", AccountID: "acct_1"}, managementOwner: "acct_1", managementShared: true}
	service := testService(t, repository, sequentialID())
	label := "helloworld"
	if _, err := service.PatchTunnel(context.Background(), testRequest(false), "tun_1", PatchTunnelRequest{
		Subdomain: &label, MutationInput: MutationInput{ExpectedGeneration: 1, IdempotencyKey: "op_patch_1", RequestHash: testHash()},
	}); !errors.Is(err, previewtunnelapi.ErrForbidden) || repository.patch.TunnelID != "" {
		t.Fatalf("shared patch = %v (reached repository: %v)", err, repository.patch.TunnelID != "")
	}
	if _, err := service.CheckSubdomain(context.Background(), testRequest(false), "tun_1", label); !errors.Is(err, previewtunnelapi.ErrForbidden) {
		t.Fatalf("shared check = %v", err)
	}
}

// The shipped reserved list must load: an invalid entry stops the server at startup.
func TestDefaultReservedSubdomainsAreValidLabels(t *testing.T) {
	list := config.Default().Tunnel.ReservedSubdomains
	if len(list) == 0 {
		t.Fatal("no default reserved subdomains")
	}
	if _, err := reservedSet(list); err != nil {
		t.Fatal(err)
	}
}
