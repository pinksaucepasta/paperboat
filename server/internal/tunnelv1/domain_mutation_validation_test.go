package tunnelv1

import "testing"

// Verify/delete address an existing domain by its fenced ID, not a caller-supplied
// hostname or route. Creation must still supply both publication fields.
func TestExistingDomainMutationsRequireIdentityWithoutCreationFields(t *testing.T) {
	input := DomainRecord{OperationID: "op_test", AuditEventID: "aud_test", AccountID: "acct_test", TunnelID: "tun_test", DomainID: "dom_test", IdempotencyKey: "mutation-test", ActorID: "actor_test", ActorType: "user", CorrelationID: "correlation_test", ExpectedGeneration: 1, RequestHash: [32]byte{1}}
	if err := validateDomainRecord(input, true); err != nil {
		t.Fatal(err)
	}
	if err := validateDomainRecord(input, false); err == nil {
		t.Fatal("creation accepted missing route and hostname")
	}
	input.DomainID = ""
	if err := validateDomainRecord(input, true); err == nil {
		t.Fatal("mutation accepted missing domain identity")
	}
}
