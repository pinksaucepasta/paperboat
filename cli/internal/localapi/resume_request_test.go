package localapi

import (
	"testing"
	"time"
)

// A resume re-attach carries no credential: only a terminal may ask for it, and it is not an offline stream.
func TestResumeRequestNeedsNoCredentialOnlyForATerminal(t *testing.T) {
	now := time.Now()
	resume := PeerStreamRequest{Workspace: "personal", Schema: PeerStreamSchemaV1, MachineID: "machine_1", EnvironmentID: "env_1", MachineGeneration: 1, Consumer: "terminal", OperationID: "op_1", Deadline: now.Add(time.Minute), MaximumBytes: 1 << 20, Resume: true}
	if err := resume.Validate(now); err != nil {
		t.Fatalf("resume terminal request: %v", err)
	}
	if err := resume.ValidatePending(now); err != nil {
		t.Fatalf("resume terminal request, pending form: %v", err)
	}
	for name, mutate := range map[string]func(*PeerStreamRequest){
		"exec":       func(r *PeerStreamRequest) { r.Consumer = "exec" },
		"offline":    func(r *PeerStreamRequest) { r.Offline = true },
		"credential": func(r *PeerStreamRequest) { r.Credential = "token" },
	} {
		bad := resume
		mutate(&bad)
		if bad.Validate(now) == nil {
			t.Errorf("%s resume request was accepted", name)
		}
	}
}
