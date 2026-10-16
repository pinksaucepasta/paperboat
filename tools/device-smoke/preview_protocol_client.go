// This smoke client runs outside the publishing device's network. All names
// come from the preview's actual ready receipt; TLS verification stays enabled.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

func main() {
	address := flag.String("url", "", "ready HTTPS preview endpoint")
	marker := flag.String("marker", "", "synthetic origin marker")
	flag.Parse()
	if err := qualify(*address, *marker); err != nil {
		// Request errors can carry their URL, but never response or payload contents.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "PASS", "http1": true, "http2": true, "http3": true, "websocket": true, "large_body_bytes": 16 << 20})
}

func qualify(address, marker string) error {
	endpoint, err := url.Parse(address)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || marker == "" {
		return errors.New("protocol probe needs the exact ready HTTPS endpoint and marker")
	}
	address = strings.TrimRight(address, "/")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	h1 := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}}
	defer h1.CloseIdleConnections()
	h2 := &http2.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	defer h2.CloseIdleConnections()
	h3 := &http3.Transport{}
	defer h3.Close()
	for _, test := range []struct {
		version   int
		transport http.RoundTripper
	}{{1, h1}, {2, h2}, {3, h3}} {
		client := &http.Client{Transport: test.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, address+"/marker", nil)
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("HTTP/%d marker request: %w", test.version, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return fmt.Errorf("HTTP/%d response read: %w", test.version, errors.Join(readErr, closeErr))
		}
		if response.StatusCode != 200 || response.ProtoMajor != test.version || !bytes.Equal(body, []byte(marker)) {
			return fmt.Errorf("HTTP/%d exact marker/status/negotiated-version check failed (status=%d, protocol=%s)", test.version, response.StatusCode, response.Proto)
		}
	}
	if err := largeBody(ctx, &http.Client{Transport: h2}, address); err != nil {
		return err
	}
	return socketEcho(ctx, &http.Client{Transport: h1}, address, marker)
}

func largeBody(ctx context.Context, client *http.Client, address string) error {
	block := make([]byte, 256)
	for i := range block {
		block[i] = byte(i)
	}
	payload := bytes.Repeat(block, 65536)
	expected := sha256.Sum256(payload)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, address+"/digest", bytes.NewReader(payload))
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("large request body: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return fmt.Errorf("large body receipt: %w", err)
	}
	if response.StatusCode != 200 || string(body) != hex.EncodeToString(expected[:]) {
		return errors.New("large request body changed its exact digest or status")
	}
	return nil
}

func socketEcho(ctx context.Context, client *http.Client, address, marker string) error {
	socket, response, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(address, "https")+"/ws", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return fmt.Errorf("WebSocket handshake: %w", err)
	}
	defer socket.CloseNow()
	socket.SetReadLimit(1 << 20)
	for _, payload := range [][]byte{[]byte(marker + "_λ船"), bytes.Repeat([]byte{0, 255, 1, 128}, 128<<10)} {
		if err := socket.Write(ctx, websocket.MessageBinary, payload); err != nil {
			return fmt.Errorf("WebSocket write: %w", err)
		}
		kind, body, err := socket.Read(ctx)
		if err != nil {
			return fmt.Errorf("WebSocket read: %w", err)
		}
		if kind != websocket.MessageBinary || !bytes.Equal(body, payload) {
			return errors.New("WebSocket changed its exact binary/Unicode message")
		}
	}
	return nil
}
