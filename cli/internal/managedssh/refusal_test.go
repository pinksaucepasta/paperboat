package managedssh

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

// A stream that ends before any byte is named as a Paperboat transport close, not an sshd limit.
func TestReadServerStartNamesABareEOFBeforeTheBanner(t *testing.T) {
	err := ReadServerStart(bufio.NewReader(strings.NewReader("")))
	if !errors.Is(err, ErrSSHServerClosed) {
		t.Fatalf("err = %v, want ErrSSHServerClosed", err)
	}
	message := err.Error()
	if !strings.Contains(message, "Paperboat's connection to the machine closed before its SSH server answered") ||
		!strings.Contains(message, "pb doctor") || strings.Contains(message, "MaxStartups") {
		t.Fatalf("message = %q", message)
	}
}
