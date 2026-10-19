//go:build js && wasm

package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall/js"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

const (
	websocketQueueDepth = 32
	websocketWriteLimit = 1 << 20
	websocketWriteWait  = 10 * time.Second
	terminalInputLimit  = protocol.MaxBinaryFrame - 13
	maximumColumns      = 500
	maximumRows         = 200
)

var browserRuntime = struct {
	sync.Mutex
	identities  map[string]*browserIdentity
	connections map[string]*browserTerminal
}{identities: make(map[string]*browserIdentity), connections: make(map[string]*browserTerminal)}

type terminalConfig struct {
	URL               string
	Subprotocols      []string
	OwnerAccountID    string
	MachineID         string
	RootKeyID         string
	RootPublicKey     string
	RootFingerprint   string
	TerminalSessionID string
	Role              string
	FromSequence      uint64
	AtLiveBoundary    bool
}

type browserTerminal struct {
	id       string
	identity *browserIdentity
	callback js.Value
	config   terminalConfig

	mu              sync.Mutex
	websocket       *browserWSConn
	tls             *tls.Conn
	streamID        uint32
	attachmentID    string
	inputSequence   uint64
	resizeSequence  uint64
	cursorRequestID string
	fileRequests    map[string]chan protocol.Frame
	fileDone        chan struct{}
	fileCloseOnce   sync.Once
	filesAvailable  bool
	attached        bool
	stopping        atomic.Bool
	ended           atomic.Bool
	stopOnce        sync.Once
	keyEvents       chan browserbroadcast.Epoch
	writes          *terminalWriterQueue
}

type websocketMessage struct {
	data   []byte
	binary bool
}

type browserWSConn struct {
	socket js.Value

	messages   chan websocketMessage
	broadcasts *terminalReceiveQueue
	opened     chan struct{}
	done       chan struct{}
	failure    chan error

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	readBuffer    []byte
	readOffset    int
	closeErr      error
	openOnce      sync.Once
	doneOnce      sync.Once
	writeMu       sync.Mutex
	closeOnce     sync.Once
	handlers      []js.Func
}

func main() {
	api := js.Global().Get("Object").New()
	api.Set("createIdentity", js.FuncOf(apiCreateIdentity))
	api.Set("inspectRoot", js.FuncOf(apiInspectRoot))
	api.Set("discardIdentity", js.FuncOf(apiDiscardIdentity))
	api.Set("connect", js.FuncOf(apiConnect))
	api.Set("sendInput", js.FuncOf(apiSendInput))
	api.Set("resize", js.FuncOf(apiResize))
	api.Set("ack", js.FuncOf(apiAck))
	api.Set("cursor", js.FuncOf(apiCursor))
	api.Set("hashFile", js.FuncOf(apiHashFile))
	api.Set("fileRequest", js.FuncOf(apiFileRequest))
	api.Set("leave", js.FuncOf(apiLeave))
	js.Global().Set("paperboatBrowserTerminal", api)
	select {}
}

func getConnection(id string) *browserTerminal {
	browserRuntime.Lock()
	defer browserRuntime.Unlock()
	return browserRuntime.connections[id]
}

func randomID(noun string) (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return noun + "_" + id.String(), nil
}

func stringProperty(value js.Value, name string) string {
	property := value.Get(name)
	if property.Type() != js.TypeString {
		return ""
	}
	return property.String()
}

func bytesFromJS(value js.Value) ([]byte, error) {
	if value.Type() == js.TypeString {
		return []byte(value.String()), nil
	}
	if value.Type() != js.TypeObject || value.IsNull() {
		return nil, errors.New("expected byte array or string")
	}
	array := js.Global().Get("Uint8Array").New(value)
	length := array.Get("byteLength").Int()
	if length < 0 || length > 1<<20 {
		return nil, errors.New("byte array is too large")
	}
	data := make([]byte, length)
	if js.CopyBytesToGo(data, array) != length {
		return nil, errors.New("byte array could not be copied")
	}
	return data, nil
}

func jsByteArray(data []byte) js.Value {
	array := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(array, data)
	return array
}

func jsObject(values map[string]any) js.Value {
	result := js.Global().Get("Object").New()
	for name, value := range values {
		switch typed := value.(type) {
		case js.Value:
			result.Set(name, typed)
		case json.RawMessage:
			parsed := js.Global().Get("JSON").Call("parse", string(typed))
			result.Set(name, parsed)
		default:
			result.Set(name, js.ValueOf(typed))
		}
	}
	return result
}

func errorObject(code, message string) js.Value {
	return js.ValueOf(map[string]any{"error": code, "message": message})
}

var _ net.Conn = (*browserWSConn)(nil)
