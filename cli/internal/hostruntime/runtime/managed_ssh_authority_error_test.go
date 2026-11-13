package runtime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
)

// Paperboat being unreachable keeps the managed keys whatever shape the network failure takes; only an explicit answer
// (refused, signed out) removes them. A DNS failure used to count as an answer: the keys went and every client was
// refused for the length of the outage.
func TestManagedSSHAuthorityNetworkFailuresKeepTheKeys(t *testing.T) {
	for name, err := range map[string]error{
		"connect refused or dropped": &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: invalid argument")},
		"lookup failed":              &net.DNSError{Err: "no such host", Name: "control.example.test", IsNotFound: true},
		"lookup timed out":           &net.DNSError{Err: "i/o timeout", Name: "control.example.test", IsTimeout: true},
		"client timeout":             &url.Error{Op: "Post", URL: "https://control.example.test/", Err: context.DeadlineExceeded},
		"wrapped lookup failure":     errors.Join(errors.New("authority"), &url.Error{Op: "Post", URL: "https://control.example.test/", Err: &net.DNSError{Err: "server misbehaving", Name: "control.example.test"}}),
		"server unavailable":         &clientapi.APIError{Status: http.StatusServiceUnavailable},
		"account over the key cap":   &clientapi.APIError{Status: http.StatusConflict, Code: "managed_ssh_key_capacity"},
	} {
		if !retryableManagedSSHAuthorityError(err) {
			t.Errorf("%s: treated as an answer, the keys would be removed", name)
		}
	}
	for name, err := range map[string]error{
		"signed out":       clientapi.ErrUnauthenticated,
		"explicit refusal": &clientapi.APIError{Status: http.StatusForbidden},
		"canceled":         context.Canceled,
		"other conflict":   &clientapi.APIError{Status: http.StatusConflict, Code: "operation_conflict"},
		"plain error":      errors.New("the answer is not valid"),
	} {
		if retryableManagedSSHAuthorityError(err) {
			t.Errorf("%s: kept the keys, want them removed", name)
		}
	}
}
