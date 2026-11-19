package edgehttp

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/terminalprotocol"
	"net/http"
	"strings"
)

const browserTerminalSubprotocol = terminalprotocol.BrowserSubprotocol
const browserTerminalTicketPrefix = terminalprotocol.BrowserTicketPrefix

const (
	browserTerminalTLSDiscriminator          byte = 0x00
	browserTerminalSharedOutputDiscriminator byte = 0x01
	// browserTerminalLaggedDiscriminator alone tells the browser its shared output was dropped behind a flood:
	// it asks the host for a fresh screen on its stream and keeps the connection.
	browserTerminalLaggedDiscriminator byte = 0x02
	// browserTerminalFileDiscriminator, browser to edge only, marks a TLS record the browser wrote for a FILE transfer request (an upload
	// to the machine's Inbox). The records are as opaque as any other (the edge never decrypts); the marker is the browser's own, set
	// by its writer for the one kind of request it sends for file transfers. The edge counts and paces what carries it with the
	// workspace's allowance, and strips it like the TLS marker. Limit: a modified browser can send file bytes unmarked; they still pass
	// through the terminal cap, so nothing goes through faster than that.
	browserTerminalFileDiscriminator byte = terminalprotocol.BrowserFileEnvelope
)

var ErrBrowserTerminalMessage = errors.New("invalid browser terminal message")

// browserTerminalTLSMessagePayload returns the TLS record of a browser message and whether the browser marked it as a file transfer's.
func browserTerminalTLSMessagePayload(message []byte) (record []byte, file bool, err error) {
	if len(message) < 2 || len(message) > browserTerminalMaxRecordBytes+1 || (message[0] != browserTerminalTLSDiscriminator && message[0] != browserTerminalFileDiscriminator) {
		return nil, false, ErrBrowserTerminalMessage
	}
	return message[1:], message[0] == browserTerminalFileDiscriminator, nil
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
