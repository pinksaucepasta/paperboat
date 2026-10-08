package server

import (
	"context"
	"fmt"
	clienttransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNativeFileTransferHistoryPagesAndSessionIsolation(t *testing.T) {
	handler, durable := fileTransferTestHandler(t)
	now := time.Now().UTC()
	for i := 0; i < 202; i++ {
		item := store.FileTransfer{ID: fmt.Sprintf("ft_%03d", i), BatchID: fmt.Sprintf("fb_%03d", i), SourceMachineID: "machine_client", DestinationMachineID: "machine_host", InitiatingUserID: "user_1", SessionID: "ses_1", Basename: "entry.txt", SHA256: transferDigest(nil), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if i == 201 {
			item.SourceMachineID = "another_machine"
		}
		if err := durable.CreateFileTransfers(t.Context(), []store.FileTransfer{item}); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := httptest.NewServer(handler)
	defer endpoint.Close()
	client, err := clienttransfer.NewNativeClient(endpoint.URL+"/v1/file-transfers", clienttransfer.Auth{Token: "token"}, clienttransfer.Binding{SourceMachineID: "machine_client", DestinationMachineID: "machine_host", InitiatingUserID: "user_1"}, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	items, err := client.List(t.Context(), "ses_1", 200)
	if err != nil || len(items) != 201 {
		t.Fatalf("history count=%d err=%v", len(items), err)
	}
	if _, err := client.ListPage(t.Context(), "another_session", 50, 0, "", ""); err == nil {
		t.Fatal("cross-session discovery accepted")
	}
	for _, suffix := range []string{"&offset=-1", "&limit=201", "&state=invalid", "&q=%00"} {
		request := transferRequest(http.MethodGet, endpoint.URL+"/v1/file-transfers?session_id=ses_1"+suffix, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request") {
			t.Fatalf("invalid page %s: %d %s", suffix, response.Code, response.Body.String())
		}
	}
	filtered, err := client.ListPage(t.Context(), "ses_1", 50, 0, "FT_001", "created")
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].TransferID != "ft_001" {
		t.Fatalf("filtered=%#v err=%v", filtered, err)
	}
}
