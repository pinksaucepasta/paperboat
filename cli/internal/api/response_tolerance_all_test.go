package api

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/recording"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
	"github.com/pinksaucepasta/paperboat/shared/wirejson"
)

// filled builds a value with every field set and every list holding one element, so its JSON has an object at every
// level a response can have (the zero value has none: empty lists hide the nested objects). Depth stops cycles.
func filled(kind reflect.Type, depth int) reflect.Value {
	value := reflect.New(kind).Elem()
	if depth > 5 {
		return value
	}
	switch {
	case kind == reflect.TypeOf(time.Time{}):
		value.Set(reflect.ValueOf(time.Now().UTC()))
	case kind == reflect.TypeOf(json.RawMessage{}):
		value.Set(reflect.ValueOf(json.RawMessage(`{"k":"v"}`)))
	case kind.Kind() == reflect.Pointer:
		value.Set(filled(kind.Elem(), depth+1).Addr())
	case kind.Kind() == reflect.Struct:
		for i := 0; i < kind.NumField(); i++ {
			if kind.Field(i).IsExported() {
				value.Field(i).Set(filled(kind.Field(i).Type, depth+1))
			}
		}
	case kind.Kind() == reflect.Slice:
		value.Set(reflect.Append(value, filled(kind.Elem(), depth+1)))
	case kind.Kind() == reflect.Map:
		value.Set(reflect.MakeMap(kind))
		value.SetMapIndex(filled(kind.Key(), depth+1), filled(kind.Elem(), depth+1))
	case kind.Kind() == reflect.Interface:
		value.Set(reflect.ValueOf(map[string]any{"k": "v"}))
	case kind.Kind() == reflect.String:
		value.SetString("x")
	case kind.Kind() == reflect.Bool:
		value.SetBool(true)
	case kind.Kind() >= reflect.Int && kind.Kind() <= reflect.Int64:
		value.SetInt(1)
	case kind.Kind() >= reflect.Uint && kind.Kind() <= reflect.Uint64:
		value.SetUint(1)
	case kind.Kind() == reflect.Float32 || kind.Kind() == reflect.Float64:
		value.SetFloat(1)
	}
	return value
}

// Every response type the client decodes (the data of each call in this package that has one), filled in at every
// level and then given a field no client has heard of in every object: both decoders the client uses must accept it.
// A type that cannot decode its own filled form (a strict custom decoder) is named in the log, not skipped silently.
func TestEveryResponseTypeIgnoresFieldsAddedByANewerServer(t *testing.T) {
	for _, prototype := range []any{
		Favorite{}, ConfigRepository{}, ConfigRepositoryCandidate{}, ConfigAssignment{}, ConfigMachineRule{}, ConfigTeamDefault{},
		ConfigWarningFacts{}, ConfigOperation{}, E2EEBootstrapResult{}, PendingEndpointIdentity{},
		MachineEnrollmentStart{}, UserMachine{}, AuthenticatedMachineInstallation{}, MachineControlCredential{}, Me{},
		AvailabilityPolicy{}, MachineCapabilityPolicy{}, ManagedSSHTarget{}, ManagedSSHHostKeySet{}, ManagedSSHUserCAs{},
		ManagedSSHCertificate{}, SendPrepareResponse{}, TransferDestinationDefault{}, ConnectionDescriptor{}, ExecDescriptor{},
		FileTransfer{}, TerminalSession{}, TerminalSessionCloseResult{}, ConfigComparisonDescriptor{}, HostedEdgePage{},
		EnvironmentVariableCollection{}, EnvVault{}, GitHubNativeLink{}, InspectorCredential{}, IPRules{}, LazyPolicy{},
		MachineServicesMachine{}, MachineLatency{}, PasswordVaultHead{}, PeerNetworkRegistrationResult{},
		PeerNetworkConfigurationResult{}, Policy{}, PolicyValidation{}, PolicyExplanation{}, PolicyVersionPage{},
		APITokenCreated{}, APITokenPage{}, APIToken{}, MachineTagsView{}, PortNameRecord{}, FrontDesk{}, Recipe{},
		RecipeMachine{}, RecipeUpdated{}, RecipeRevoked{}, RecipeMachineState{}, SelfhostInstallation{}, SelfhostPool{},
		ActivityPage{}, TeamIdentity{}, TeamInboxPolicy{}, TeamInboxRequest{}, Team{}, TeamCheckout{}, TeamInvitation{},
		TeamPage[TeamMember]{}, TeamPage[TeamGrant]{}, TeamPage[TeamMachineBinding]{}, SharedTerminalSession{},
		TunnelPage{}, Tunnel{}, TunnelMutation{}, TunnelRoutePage{}, TunnelRoute{}, TunnelRouteMutation{}, TunnelDomainPage{},
		TunnelDomain{}, TunnelDomainMutation{}, TunnelOperation{}, TunnelHealth{}, TunnelLogPage{}, TunnelEventPage{},
		VaultScopeState{}, VaultSharingState{}, VaultTeamState{}, VaultGrantState{}, VaultPersonalScopes{}, VaultScopeDocument{},
		VaultLayerRecipient{}, VaultLayerDelivery{}, VaultLayerContext{}, VaultRecordState{}, VaultRecordsResult{}, VaultRecordsPage{},
		UpdatePolicy{}, recording.List{}, recording.Replay{}, recording.KeyState{}, recording.Key{}, teambucket.Config{},
	} {
		kind := reflect.TypeOf(prototype)
		t.Run(kind.String(), func(t *testing.T) {
			base, err := json.Marshal(filled(kind, 0).Interface())
			if err != nil {
				t.Fatal(err)
			}
			if err := wirejson.Decode(base, reflect.New(kind).Interface()); err != nil {
				t.Logf("%s cannot decode its own filled form, so newer fields are not tried: %v", kind, err)
				t.Skip()
			}
			var generic any
			if err := json.Unmarshal(base, &generic); err != nil {
				t.Fatal(err)
			}
			extended, err := json.Marshal(addUnknownFields(generic))
			if err != nil {
				t.Fatal(err)
			}
			if err := wirejson.Decode(extended, reflect.New(kind).Interface()); err != nil {
				t.Fatalf("wirejson refused a response with added fields: %v", err)
			}
			if err := json.Unmarshal(extended, reflect.New(kind).Interface()); err != nil {
				t.Fatalf("json refused a response with added fields: %v", err)
			}
		})
	}
}
