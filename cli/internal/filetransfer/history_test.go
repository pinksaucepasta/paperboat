package filetransfer

import (
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTransferHistoryRejectsForeignBindingsAndBrokenPages(t *testing.T) {
	for _, kind := range []string{"source", "destination", "user", "session", "cursor", "empty"} {
		t.Run(kind, func(t *testing.T) {
			manifest := Manifest{TransferID: "ft_1", SourceMachineID: "source", DestinationMachineID: "destination", InitiatingUserID: "user", SessionID: "session"}
			switch kind {
			case "source":
				manifest.SourceMachineID = "foreign"
			case "destination":
				manifest.DestinationMachineID = "foreign"
			case "user":
				manifest.InitiatingUserID = "foreign"
			case "session":
				manifest.SessionID = "foreign"
			}
			page := HistoryPage{Items: []Manifest{manifest}, Pagination: protocol.FileTransferPagination{Limit: 50, Offset: 0, Total: 1}}
			if kind == "cursor" {
				next := 0
				page.Pagination = protocol.FileTransferPagination{Limit: 50, Offset: 0, Total: 1, NextOffset: &next}
			}
			if kind == "empty" {
				next := 200
				page.Items = nil
				page.Pagination.NextOffset = &next
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(page) }))
			defer server.Close()
			client := NewClient(server.URL, Auth{Token: "token"}, Binding{SourceMachineID: "source", DestinationMachineID: "destination", InitiatingUserID: "user"}, server.Client())
			if _, err := client.ListPage(t.Context(), "session", 50, 0, "", ""); err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}
