//go:build js && wasm

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"syscall/js"
	"time"
)

func apiCompareConfig(_ js.Value, args []js.Value) any {
	if len(args) != 3 || args[1].Type() != js.TypeObject || args[2].Type() != js.TypeString {
		return errorObject("invalid_request", "Invalid config comparison request.")
	}
	config, err := parseBrowserTransportConfig(args[1], false)
	if err != nil {
		return errorObject("invalid_request", "Invalid config comparison transport.")
	}
	var input struct {
		compareRequest
		OperationID string `json:"operation_id"`
	}
	request := &input.compareRequest
	if json.Unmarshal([]byte(args[2].String()), &input) != nil || request.Action != "compare" || input.OperationID == "" || request.AssignmentID == "" || request.AssignmentVersion < 1 || request.Path == "" || len(request.ConflictRevision) != 64 || len(request.ExpectedRemoteRevision) != 40 {
		return errorObject("invalid_request", "Invalid config comparison binding.")
	}
	identityID := args[0].String()
	browserRuntime.Lock()
	identity := browserRuntime.identities[identityID]
	delete(browserRuntime.identities, identityID)
	browserRuntime.Unlock()
	if identity == nil {
		return errorObject("identity_missing", "The temporary browser key expired. Retry the comparison.")
	}
	executor := js.FuncOf(func(_ js.Value, callbacks []js.Value) any {
		resolve := callbacks[0]
		go func() {
			defer zero(identity.PrivateKey)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			metadata, local, managed, err := readConfigComparison(ctx, identity, config, *request, input.OperationID)
			defer clear(local)
			defer clear(managed)
			if err != nil {
				resolve.Invoke(errorObject("comparison_failed", "The conflict comparison could not be verified. Refresh the conflict and retry."))
				return
			}
			raw, _ := json.Marshal(metadata)
			resolve.Invoke(jsObject(map[string]any{"metadata": string(raw), "local": jsByteArray(local), "managed": jsByteArray(managed)}))
		}()
		return nil
	})
	promise := js.Global().Get("Promise").New(executor)
	executor.Release()
	return promise
}
func readConfigComparison(ctx context.Context, identity *browserIdentity, config terminalConfig, request compareRequest, operationID string) (compareMetadata, []byte, []byte, error) {
	metadata := compareMetadata{}
	ws, err := newBrowserWSConn(config.URL, config.Subprotocols)
	if err != nil {
		return metadata, nil, nil, err
	}
	defer ws.Close()
	if err = ws.waitOpen(ctx); err != nil {
		return metadata, nil, nil, err
	}
	initial, err := ws.readInitialMessage(ctx)
	if err != nil || !initial.binary {
		return metadata, nil, nil, errInvalidPeerIdentity
	}
	root, _ := base64.RawURLEncoding.Strict().DecodeString(config.RootPublicKey)
	peer, err := verifyIdentityEnvelope(initial.data, ed25519.PublicKey(root), config.RootKeyID, config.OwnerAccountID, config.MachineID, time.Now())
	if err != nil {
		return metadata, nil, nil, err
	}
	if ws.socket.Get("protocol").String() != "paperboat.browser-config-compare.e2ee.v1" {
		return metadata, nil, nil, errInvalidPeerIdentity
	}
	conn := tls.Client(ws, tlsClientConfig(identity, peer, time.Now))
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return metadata, nil, nil, err
	}
	if err = conn.HandshakeContext(ctx); err != nil {
		return metadata, nil, nil, err
	}
	return readConfigComparisonContent(conn, request, operationID)
}
