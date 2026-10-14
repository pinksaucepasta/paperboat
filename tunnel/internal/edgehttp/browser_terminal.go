package edgehttp

import (
	"errors"
	"net/http"
	"strings"
)

const browserTerminalSubprotocol = "paperboat.browser-terminal.e2ee.v1"
const browserTerminalTicketPrefix = "pb-ticket."

const (
	browserTerminalTLSDiscriminator          byte = 0x00
	browserTerminalSharedOutputDiscriminator byte = 0x01
)

var ErrBrowserTerminalMessage = errors.New("invalid browser terminal message")

func browserTerminalTLSMessagePayload(message []byte) ([]byte, error) {
	if len(message) < 2 || len(message) > browserTerminalMaxRecordBytes+1 || message[0] != browserTerminalTLSDiscriminator {
		return nil, ErrBrowserTerminalMessage
	}
	return message[1:], nil
}

func browserTerminalSharedOutputMessagePayload(record []byte) ([]byte, error) {
	if len(record) == 0 || len(record) > browserTerminalMaxRecordBytes {
		return nil, ErrBrowserTerminalMessage
	}
	message := make([]byte, len(record)+1)
	message[0] = browserTerminalSharedOutputDiscriminator
	copy(message[1:], record)
	return message, nil
}

func browserTerminalTLSMessage(record []byte) ([]byte, error) {
	if len(record) == 0 || len(record) > browserTerminalMaxRecordBytes {
		return nil, ErrBrowserTerminalMessage
	}
	message := make([]byte, len(record)+1)
	message[0] = browserTerminalTLSDiscriminator
	copy(message[1:], record)
	return message, nil
}

func browserTerminalTicket(header http.Header) (string, bool) {
	return browserOperationTicket(header, browserTerminalSubprotocol)
}
func browserOperationTicket(header http.Header, expectedProtocol string) (string, bool) {
	var ticket string
	protocol := false
	for _, field := range header.Values("Sec-WebSocket-Protocol") {
		for _, raw := range strings.Split(field, ",") {
			part := strings.TrimSpace(raw)
			switch {
			case part == expectedProtocol:
				if protocol {
					return "", false
				}
				protocol = true
			case strings.HasPrefix(part, browserTerminalTicketPrefix):
				if ticket != "" || len(part) != len(browserTerminalTicketPrefix)+43 {
					return "", false
				}
				ticket = strings.TrimPrefix(part, browserTerminalTicketPrefix)
			default:
				return "", false
			}
		}
	}
	return ticket, protocol && ticket != ""
}
