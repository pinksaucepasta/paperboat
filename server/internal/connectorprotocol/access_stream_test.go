package connectorprotocol

import (
	"net/http"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func privateAccessTestRequest(now time.Time) wire.PrivateAccessRequest {
	return wire.PrivateAccessRequest{
		AccountID: "account_1", ResourceKind: "preview", ResourceID: "preview_1", RouteID: "route_1",
		Audience: "paperboat-preview-http", MachineID: "machine_1", SessionID: "installation_4",
		InstallationGeneration: 4, ExpiresAt: now.Add(time.Minute), Nonce: "nonce_1", OperationID: "operation_1",
		CarrierSessionID: "session_1", RouteGeneration: 1, ProcessGeneration: 2, ConfigGeneration: 3,
		SessionGeneration: 4, AssignmentGeneration: 5, EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1",
		Protocol: "http", Method: http.MethodConnect, Host: "private.preview.example.test", Path: "/",
		IdempotencyKey: "access_1", RequestID: "request_1", CorrelationID: "correlation_1",
	}
}
