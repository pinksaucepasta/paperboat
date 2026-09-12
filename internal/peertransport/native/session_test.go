package native

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

func TestCapabilityForConsumerIsExact(t *testing.T) {
	want := map[string]string{
		"terminal":          "terminal",
		"exec":              "exec",
		"ssh":               "managed_ssh",
		"file_transfer":     "file_transfer",
		"file_transfer_key": "file_transfer",
		"private_http":      "private_access",
		"private_tcp":       "private_access",
	}
	for consumer, capability := range want {
		if got := capabilityForConsumer(consumer); got != capability {
			t.Fatalf("consumer %q capability=%q want=%q", consumer, got, capability)
		}
	}
	if got := capabilityForConsumer("preview_manage"); got != "" {
		t.Fatalf("management-only consumer received native capability %q", got)
	}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(value []byte) (int, error) {
	if len(value) > 1 {
		value = value[:1]
	}
	return w.Buffer.Write(value)
}

func TestWriteHeaderCompletesShortWrites(t *testing.T) {
	writer := new(shortWriter)
	if err := writeHeader(writer, []byte("header")); err != nil {
		t.Fatal(err)
	}
	encoded, err := readHeader(bytes.NewReader(writer.Bytes()))
	if err != nil || string(encoded) != "header" {
		t.Fatalf("header=%q err=%v", encoded, err)
	}
}

func TestLimitedConnRejectsBytesBeyondGrant(t *testing.T) {
	connection := newLimitedConn(stubConn{Reader: bytes.NewReader([]byte("four")), Writer: io.Discard}, 3)
	value := make([]byte, 8)
	read, err := connection.Read(value)
	if err != nil || string(value[:read]) != "fou" {
		t.Fatalf("read=%q err=%v", value[:read], err)
	}
	if _, err = connection.Read(value); !errors.Is(err, streamauth.ErrInvalid) {
		t.Fatalf("read beyond grant: %v", err)
	}
	if _, err = connection.Write([]byte("four")); !errors.Is(err, streamauth.ErrInvalid) {
		t.Fatalf("write beyond grant: %v", err)
	}
}

type stubConn struct {
	io.Reader
	io.Writer
}

func (stubConn) Close() error                     { return nil }
func (stubConn) LocalAddr() net.Addr              { return directAddr("local") }
func (stubConn) RemoteAddr() net.Addr             { return directAddr("remote") }
func (stubConn) SetDeadline(time.Time) error      { return nil }
func (stubConn) SetReadDeadline(time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(time.Time) error { return nil }
