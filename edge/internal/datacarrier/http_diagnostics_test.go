package datacarrier

import (
	"context"
	"testing"
)

func TestCarrierDiagnosticSeparatesConnectionFromServerFailure(t *testing.T) {
	for line, want := range map[string]error{
		"http2: server: error reading preface from client 192.0.2.1:1: unexpected EOF\n": errCarrierConnection,
		"timeout waiting for SETTINGS frames from 192.0.2.1:1\n":                         errCarrierConnection,
		"http: Accept error: too many open files; retrying in 5ms\n":                     ErrCarrierHTTPServer,
	} {
		var got error
		service := &HTTPService{ctx: t.Context(), onFailure: func(_ context.Context, err error) { got = err }}
		if _, err := (carrierHTTPDiagnosticWriter{service}).Write([]byte(line)); err != nil || got != want {
			t.Fatalf("%q: got %v, want %v (%v)", line, got, want, err)
		}
	}
}
