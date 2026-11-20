package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
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

// 40 pages of 200 records: above the former 32-page (6400 record) cap.
func TestVaultRecordsReadsMoreThan32Pages(t *testing.T) {
	const pages, perPage = 40, 200
	coordinate := VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account-one", MachineID: "machine-one"}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		records := make([]VaultRecordState, perPage)
		for i := range records {
			records[i] = VaultRecordState{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account-one", MachineID: "machine-one", RecordID: fmt.Sprintf("r%d", page*perPage+i), Sequence: 1}
		}
		next := ""
		if page+1 < pages {
			next = strconv.Itoa(page + 1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"data": VaultRecordsPage{Records: records, Sequence: 1, NextCursor: next}})
	}))
	defer server.Close()
	out, err := New(server.URL, config.Credential{}, server.Client()).VaultRecords(context.Background(), coordinate, 0)
	if err != nil || len(out.Records) != pages*perPage {
		t.Fatalf("records over 32 pages were refused: %d records, %v", len(out.Records), err)
	}
}
