package localdaemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// F21-02: the exec credential lives 2 min on the server clock. A client clock 10 min ahead read it as
// expired and refused every exec; 10 min behind it would have outlived its server expiry locally.
func TestExecDescriptorIsJudgedByTheServerClock(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	for name, clientAhead := range map[string]time.Duration{"client 10 min ahead": 10 * time.Minute, "client 10 min behind": -10 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			serverNow := func() time.Time { return time.Now().Add(-clientAhead).UTC() }
			source := execDescriptorSource(t, serverNow, 2*time.Minute)
			result, err := source.IssuePeerStream(context.Background(), execPendingRequest())
			if err != nil || result.Credential != "operation-token" {
				t.Fatalf("exec descriptor refused under %s: %v", name, err)
			}
			// The daemon stages the stream on the same deadline: it must hold about the 2 server minutes.
			staged, err := admission.Stage(context.Background(), result.Deadline)
			if err != nil {
				t.Fatalf("staging refused the fresh credential: %v", err)
			}
			defer staged.Abort()
			if _, err := staged.Accept(); err != nil {
				t.Fatalf("accept refused the fresh credential: %v", err)
			}
			if left := serverclock.Until(result.Deadline); left < time.Minute || left > 2*time.Minute+5*time.Second {
				t.Fatalf("credential has %v left on the server clock, want about 2 min", left)
			}
		})
	}
}

// Past the bound (a credential already expired on the corrected clock) the refusal is named.
func TestAnExpiredExecDescriptorIsNamed(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	source := execDescriptorSource(t, func() time.Time { return time.Now().UTC() }, -time.Second)
	_, err := source.IssuePeerStream(context.Background(), execPendingRequest())
	var expired localapi.CredentialExpiredError
	if !errors.As(err, &expired) || expired.LocalAPICode() != "credential_expired" {
		t.Fatalf("expired descriptor error = %v, want CredentialExpiredError", err)
	}
}

func execDescriptorSource(t *testing.T, serverNow func() time.Time, lifetime time.Duration) AuthenticatedMachineSource {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		expires := serverNow().Add(lifetime).Format(time.RFC3339Nano)
		writer.Header().Set("Date", serverNow().Format(http.TimeFormat))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"data":{"operation_id":"operation_1","environment":{"id":"environment_1","kind":"machine","resource_id":"machine_1","state":"ready","root":"/root"},"endpoints":{"quic":"quic://example.test:443","wss":"wss://example.test/v1/runtime"},"auth":{"method":"bearer","token":"operation-token","expires_at":%q,"scopes":["exec:operate"]},"expires_at":%q}}`, expires, expires)
	}))
	t.Cleanup(server.Close)
	return AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}, SourceMachineID: "source_1", APIToken: apiTokenSession}
}

func execPendingRequest() localapi.PeerStreamRequest {
	return localapi.PeerStreamRequest{Workspace: "personal", Schema: localapi.PeerStreamSchemaV1, Consumer: "exec", MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Deadline: serverclock.Now().UTC().Add(time.Minute), MaximumBytes: 1024, Payload: json.RawMessage(`{"operation_id":"operation_1"}`)}
}
