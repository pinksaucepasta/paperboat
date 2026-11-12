// gRPC-over-h2c fixture for the X-20 tunnel check, one binary for the origin (on the device) and the client (on the controller):
//
//	xpc_grpc serve ADDR            h2c server: /xpc.Echo/Unary answers one framed message, /xpc.Echo/Stream three, both with
//	                               grpc-status in the trailers; /xpc.Echo/Fail is a trailers-only response with grpc-status 5
//	xpc_grpc probe https://HOST|http://HOST:PORT    calls all three through the public tunnel over HTTP/2 and checks bodies and trailers
//
// Frames are the gRPC wire format by hand (1 flag byte, 4 length bytes, payload): no generated code, no extra dependency.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func frame(payload []byte) []byte {
	out := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	return append(out, payload...)
}

func serve(address string) error {
	mux := http.NewServeMux()
	reply := func(count int, status string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/grpc")
			if count == 0 { // an error without a message is a trailers-only response: grpc-status travels in the headers
				w.Header().Set("Grpc-Status", status)
				w.Header().Set("Grpc-Message", "xpc")
				w.WriteHeader(http.StatusOK)
				return
			}
			w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
			for i := 0; i < count; i++ {
				_, _ = w.Write(frame([]byte(fmt.Sprintf("xpc-%d", i))))
				w.(http.Flusher).Flush()
				time.Sleep(150 * time.Millisecond)
			}
			w.Header().Set("Grpc-Status", status)
			w.Header().Set("Grpc-Message", "xpc")
		}
	}
	mux.HandleFunc("/xpc.Echo/Unary", reply(1, "0"))
	mux.HandleFunc("/xpc.Echo/Stream", reply(3, "0"))
	mux.HandleFunc("/xpc.Echo/Fail", reply(0, "5"))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	go func() { time.Sleep(10 * time.Minute); os.Exit(0) }()
	return (&http.Server{Handler: h2c.NewHandler(mux, &http2.Server{})}).Serve(listener)
}

func call(client *http.Client, base, method string) (string, http.Header, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+method, bytes.NewReader(frame([]byte("ping"))))
	request.Header.Set("Content-Type", "application/grpc")
	request.Header.Set("TE", "trailers")
	response, err := client.Do(request)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err == nil && response.Trailer.Get("Grpc-Status") == "" { // trailers-only: the status is in the headers
		return string(body), response.Header, nil
	}
	return string(body), response.Trailer, err
}

func probe(base string) error {
	transport := &http2.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if strings.HasPrefix(base, "http://") { // straight to an h2c origin, to tell the origin's behaviour from the edge's
		transport.AllowHTTP = true
		transport.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	}
	client := &http.Client{Transport: transport}
	for _, test := range []struct{ method, body, status string }{
		{"/xpc.Echo/Unary", string(frame([]byte("xpc-0"))), "0"},
		{"/xpc.Echo/Stream", string(frame([]byte("xpc-0"))) + string(frame([]byte("xpc-1"))) + string(frame([]byte("xpc-2"))), "0"},
		{"/xpc.Echo/Fail", "", "5"},
	} {
		body, trailer, err := call(client, base, test.method)
		if err != nil {
			return fmt.Errorf("%s: %w", test.method, err)
		}
		if body != test.body || trailer.Get("Grpc-Status") != test.status {
			return fmt.Errorf("%s: body %q grpc-status %q, want %q and %q", test.method, body, trailer.Get("Grpc-Status"), test.body, test.status)
		}
	}
	fmt.Println(`{"grpc":"PASS","unary":true,"streaming":true,"trailers":true}`)
	return nil
}

func main() {
	var err error
	switch {
	case len(os.Args) == 3 && os.Args[1] == "serve":
		err = serve(os.Args[2])
	case len(os.Args) == 3 && os.Args[1] == "probe":
		err = probe(os.Args[2])
	default:
		err = fmt.Errorf("usage: xpc_grpc serve ADDR | probe URL")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
