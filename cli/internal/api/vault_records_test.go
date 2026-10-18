package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestPutVaultRecordsAcceptsProducerSequenceReceipt(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/environment/scopes/personal/account-one/records" || r.URL.Query().Get("workspace") != "personal" {
			t.Error("record publication changed its method or authorization coordinate")
		}
		var request VaultRecordsPut
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.OperationID != "operation-one" || len(request.Records) != 1 || request.Records[0].Envelope != "opaque-envelope" {
			t.Error("record publication changed the staged mutation")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		// The producer's VaultRecordsPage always emits after_sequence, including
		// zero on the first successful mutation; next_cursor is omitted on PUT.
		fmt.Fprint(w, `{"data":{"records":[{"record_id":"record-one","revision":1,"sequence":1}],"sequence":1,"after_sequence":0}}`)
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	out, err := client.PutVaultRecords(context.Background(), VaultLayerCoordinate{
		WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account-one",
	}, VaultRecordsPut{OperationID: "operation-one", Records: []VaultRecordMutation{{Envelope: "opaque-envelope"}}})
	if err != nil {
		var decode *ResponseDecodeError
		if errors.As(err, &decode) {
			t.Fatalf("successful producer record receipt was rejected: %v", decode.Err)
		}
		t.Fatalf("successful producer record receipt was rejected: %v", err)
	}
	if out.Sequence != 1 || len(out.Records) != 1 || out.Records[0].RecordID != "record-one" || out.Records[0].Revision != 1 {
		t.Fatalf("successful producer record receipt was lost: %+v", out)
	}
}
