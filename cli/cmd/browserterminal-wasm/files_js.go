//go:build js && wasm

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"syscall/js"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

const browserHashChunk = 32 << 10

var browserHashSlots = make(chan struct{}, 2)

// Async work must yield the JS event loop: Blob reads and encrypted replies
// arrive through that same loop. Only small chunks enter Go memory.
func browserPromise(work func() any) js.Value {
	executor := js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve := args[0]
		go func() {
			defer func() {
				if recover() != nil {
					resolve.Invoke(errorObject("file_failed", "The file request could not be completed."))
				}
			}()
			resolve.Invoke(work())
		}()
		return nil
	})
	result := js.Global().Get("Promise").New(executor)
	executor.Release()
	return result
}

func awaitBlob(promise js.Value) (js.Value, error) {
	type outcome struct {
		value js.Value
		err   error
	}
	result := make(chan outcome, 1)
	success := js.FuncOf(func(_ js.Value, args []js.Value) any { result <- outcome{value: args[0]}; return nil })
	failure := js.FuncOf(func(_ js.Value, _ []js.Value) any { result <- outcome{err: errors.New("file read failed")}; return nil })
	promise.Call("then", success, failure)
	// Blob.arrayBuffer settles when its local read completes. A dangling callback
	// cannot safely be released on a timer; worker termination owns hard shutdown.
	r := <-result
	success.Release()
	failure.Release()
	return r.value, r.err
}

func apiHashFile(_ js.Value, args []js.Value) any {
	if len(args) != 1 || args[0].Type() != js.TypeObject || args[0].Get("size").Type() != js.TypeNumber {
		return errorObject("invalid_file", "Select a file to upload.")
	}
	file := args[0]
	size := file.Get("size").Float()
	if size < 0 || size > 50<<20 || size != float64(int64(size)) {
		return errorObject("file_limit", "The selected file exceeds the upload limit.")
	}
	select {
	case browserHashSlots <- struct{}{}:
	default:
		return errorObject("file_busy", "Wait for the current file upload.")
	}
	return browserPromise(func() any {
		defer func() { <-browserHashSlots }()
		digest := sha256.New()
		for offset := int64(0); offset < int64(size); offset += browserHashChunk {
			end := min(offset+browserHashChunk, int64(size))
			buffer, err := awaitBlob(file.Call("slice", float64(offset), float64(end)).Call("arrayBuffer"))
			if err != nil {
				return errorObject("file_read_failed", "The selected file could not be read. Select it again.")
			}
			chunk, err := bytesFromJS(js.Global().Get("Uint8Array").New(buffer))
			if err != nil || int64(len(chunk)) != end-offset {
				return errorObject("file_read_failed", "The selected file changed or could not be read. Select it again.")
			}
			_, _ = digest.Write(chunk)
		}
		return js.ValueOf(map[string]any{"sha256": hex.EncodeToString(digest.Sum(nil))})
	})
}

func apiFileRequest(_ js.Value, args []js.Value) any {
	if len(args) != 2 {
		return errorObject("invalid_request", "The file request is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil {
		return errorObject("connection_closed", "Reconnect to resume the file upload.")
	}
	if connection.config.Role != "owner" {
		return errorObject("not_found_or_forbidden", "Only the terminal owner can upload files.")
	}
	encoded := js.Global().Get("JSON").Call("stringify", args[1])
	if encoded.Type() != js.TypeString || len(encoded.String()) > 48<<10 {
		return errorObject("file_limit", "The file request exceeds the upload limit.")
	}
	payload := json.RawMessage(encoded.String())
	return browserPromise(func() any { return connection.fileRequest(payload) })
}

func (connection *browserTerminal) fileRequest(payload json.RawMessage) any {
	requestID, err := randomID("file_request")
	if err != nil {
		return errorObject("file_failed", "The file request could not be created.")
	}
	operationID, err := randomID("operation")
	if err != nil {
		return errorObject("file_failed", "The file request could not be created.")
	}
	frame := protocol.Frame{Type: "request", RequestID: requestID, Version: protocol.ProtocolVersion, OperationID: operationID, Capability: "file-transfer.v1", DeadlineMS: 10000, Payload: payload}
	encoded, err := json.Marshal(frame)
	if err != nil || frame.Validate() != nil || len(encoded) > protocol.MaxStructuredFrame {
		return errorObject("invalid_request", "The file request is invalid.")
	}
	reply := make(chan protocol.Frame, 1)
	connection.mu.Lock()
	if !connection.attached || connection.stopping.Load() {
		connection.mu.Unlock()
		return errorObject("connection_closed", "Reconnect to resume the file upload.")
	}
	if !connection.filesAvailable {
		connection.mu.Unlock()
		return errorObject("capability_required", "Update Paperboat on this machine to upload files from the web terminal.")
	}
	if len(connection.fileRequests) >= 2 {
		connection.mu.Unlock()
		return errorObject("file_busy", "Wait for the current file upload.")
	}
	connection.fileRequests[requestID] = reply
	err = connection.writes.enqueue(terminalWrite{kind: appKindStructured, payloads: [][]byte{encoded}, bytes: len(encoded) + 5})
	connection.mu.Unlock()
	defer func() { connection.mu.Lock(); delete(connection.fileRequests, requestID); connection.mu.Unlock() }()
	if err != nil {
		return errorObject("file_busy", "The encrypted connection is busy. Retry the upload.")
	}
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()
	select {
	case response := <-reply:
		if response.Type == "error" {
			var body struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(response.Payload, &body) != nil {
				return errorObject("invalid_response", "The machine returned an invalid file response.")
			}
			if body.Code == "credential_expired" {
				connection.emit("state", map[string]any{"state": "reconnecting", "code": body.Code, "message": "Refreshing terminal access to resume the file upload."})
			}
			code, message := browserFileFailure(body.Code)
			return errorObject(code, message)
		}
		return js.Global().Get("JSON").Call("parse", string(response.Payload))
	case <-connection.fileDone:
		return errorObject("connection_closed", "Reconnect to resume the file upload.")
	case <-timer.C:
		return errorObject("file_timeout", "The file upload was interrupted. Reconnect to resume it.")
	}
}

func (connection *browserTerminal) deliverFileResponse(frame protocol.Frame) bool {
	if !strings.HasPrefix(frame.RequestID, "file_request_") || frame.Type != "response" && frame.Type != "error" {
		return false
	}
	connection.mu.Lock()
	pending := connection.fileRequests[frame.RequestID]
	connection.mu.Unlock()
	if pending != nil {
		select {
		case pending <- frame:
		default:
		}
	}
	return true
}
func (connection *browserTerminal) closeFileRequests() {
	connection.fileCloseOnce.Do(func() { close(connection.fileDone) })
}
