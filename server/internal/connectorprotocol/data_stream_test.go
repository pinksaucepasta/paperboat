package connectorprotocol

import (
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func testStreamOpen() wire.StreamOpen {
	return wire.StreamOpen{
		Protocol:          wire.ProtocolName,
		Version:           wire.ProtocolVersion,
		AccountID:         "acct_1",
		TunnelID:          "tunnel_1",
		ConnectorID:       "connector_1",
		SessionID:         "session_1",
		ProcessGeneration: 2,
		Generation:        3,
		RouteID:           "route_1",
		RequestID:         "request_1",
		Kind:              "http",
	}
}
