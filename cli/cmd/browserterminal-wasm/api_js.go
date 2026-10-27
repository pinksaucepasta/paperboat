//go:build js && wasm

package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"strconv"
	"syscall/js"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func apiCreateIdentity(_ js.Value, _ []js.Value) any {
	identity, err := newBrowserIdentity(time.Now())
	if err != nil {
		return errorObject("identity_generation_failed", "A temporary browser key could not be created.")
	}
	id, err := randomID("key")
	if err != nil {
		zero(identity.PrivateKey)
		return errorObject("identity_generation_failed", "A temporary browser key could not be created.")
	}
	browserRuntime.Lock()
	browserRuntime.identities[id] = identity
	browserRuntime.Unlock()
	return js.ValueOf(map[string]any{"id": id, "public_key_sha256": identity.Fingerprint})
}

func apiInspectRoot(_ js.Value, args []js.Value) any {
	if len(args) != 1 || args[0].Type() != js.TypeString {
		return errorObject("invalid_root", "The trusted owner root key is invalid.")
	}
	encoded := args[0].String()
	publicKey, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(publicKey) != encoded {
		return errorObject("invalid_root", "The trusted owner root key must be canonical unpadded base64url.")
	}
	keyID := accountRootKeyID(ed25519.PublicKey(publicKey))
	return js.ValueOf(map[string]any{"root_key_id": keyID, "fingerprint": keyID[4:]})
}

func apiDiscardIdentity(_ js.Value, args []js.Value) any {
	if len(args) > 0 {
		browserRuntime.Lock()
		identity := browserRuntime.identities[args[0].String()]
		delete(browserRuntime.identities, args[0].String())
		browserRuntime.Unlock()
		if identity != nil {
			zero(identity.PrivateKey)
		}
	}
	return nil
}

func apiConnect(_ js.Value, args []js.Value) any {
	if len(args) != 3 || args[1].Type() != js.TypeObject || args[2].Type() != js.TypeFunction {
		return errorObject("invalid_request", "The browser terminal request is invalid.")
	}
	identityID := args[0].String()
	config, err := parseTerminalConfig(args[1])
	if err != nil {
		return errorObject("invalid_request", "The browser terminal request is invalid.")
	}
	browserRuntime.Lock()
	identity := browserRuntime.identities[identityID]
	delete(browserRuntime.identities, identityID)
	browserRuntime.Unlock()
	if identity == nil {
		return errorObject("identity_missing", "The temporary browser key expired. Start the connection again.")
	}
	id, err := randomID("connection")
	if err != nil {
		zero(identity.PrivateKey)
		return errorObject("connection_failed", "The encrypted connection could not be started.")
	}
	connection := &browserTerminal{id: id, identity: identity, callback: args[2], config: config, writes: newTerminalWriterQueue(), fileRequests: make(map[string]chan protocol.Frame), inspectorCalls: make(map[string]chan protocol.Frame), fileDone: make(chan struct{})}
	browserRuntime.Lock()
	browserRuntime.connections[id] = connection
	browserRuntime.Unlock()
	go connection.run()
	return js.ValueOf(map[string]any{"id": id})
}

func apiSendInput(_ js.Value, args []js.Value) any {
	if len(args) != 2 {
		return errorObject("invalid_request", "Terminal input is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	data, err := bytesFromJS(args[1])
	if err != nil || len(data) == 0 || len(data) > 1<<20 {
		return errorObject("input_limit", "Terminal input is too large.")
	}
	if err := connection.sendInput(data); err != nil {
		return errorObject("input_failed", "Terminal input could not be sent. Reconnect before retrying.")
	}
	return nil
}

func apiResize(_ js.Value, args []js.Value) any {
	if len(args) != 3 {
		return errorObject("invalid_request", "Terminal size is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	columns, rows := args[1].Int(), args[2].Int()
	if columns < 1 || columns > maximumColumns || rows < 1 || rows > maximumRows {
		return errorObject("invalid_size", "Terminal size is outside the supported range.")
	}
	if err := connection.sendResize(uint16(columns), uint16(rows)); err != nil {
		return errorObject("resize_failed", "Terminal size could not be sent.")
	}
	return nil
}

// apiActivity tells the machine this window is now the one in use (it gained focus): the PTY size follows the
// window that was last active (F3-17). Typing counts too; this covers focusing without typing.
func apiActivity(_ js.Value, args []js.Value) any {
	if len(args) != 1 {
		return errorObject("invalid_request", "Terminal activity is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	if err := connection.sendActivity(); err != nil {
		return errorObject("activity_failed", "Terminal activity could not be sent.")
	}
	return nil
}

// apiResync asks the machine for a fresh screen, as a lag does: the page dropped its backlog (tmux's tty.c
// tty_block_maybe drops and redraws). A request already pending is joined, not repeated.
func apiResync(_ js.Value, args []js.Value) any {
	if len(args) != 1 {
		return errorObject("invalid_request", "Terminal resync is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil || !connection.markLagged() {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	return nil
}

func apiAck(_ js.Value, args []js.Value) any {
	if len(args) != 2 {
		return errorObject("invalid_request", "Terminal acknowledgement is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	nextSequence, err := strconv.ParseUint(args[1].String(), 10, 64)
	if err != nil {
		return errorObject("invalid_request", "Terminal acknowledgement is invalid.")
	}
	if err := connection.sendAck(nextSequence); err != nil {
		return errorObject("ack_failed", "Terminal replay could not be acknowledged.")
	}
	return nil
}

func apiCursor(_ js.Value, args []js.Value) any {
	if len(args) != 1 {
		return errorObject("invalid_request", "Terminal cursor request is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	requestID, err := connection.sendCursor()
	if err != nil {
		return errorObject("cursor_failed", "The terminal position could not be requested.")
	}
	return js.ValueOf(map[string]any{"request_id": requestID})
}

func apiProbe(_ js.Value, args []js.Value) any {
	if len(args) != 1 {
		return errorObject("invalid_request", "Terminal probe is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "The encrypted terminal connection is closed.")
	}
	requestID, err := connection.sendProbe()
	if err != nil {
		return errorObject("probe_failed", "The terminal connection could not be checked.")
	}
	return js.ValueOf(map[string]any{"request_id": requestID})
}

func apiLeave(_ js.Value, args []js.Value) any {
	if len(args) == 0 {
		return nil
	}
	connection := getConnection(args[0].String())
	if connection != nil {
		connection.leave()
	}
	return nil
}
