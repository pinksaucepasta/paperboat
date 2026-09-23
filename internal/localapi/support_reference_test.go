package localapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type supportRoundTripFunc func(*http.Request) (*http.Response, error)

func (f supportRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestSupportReferenceTransportPropagatesInvocationReference(t *testing.T) {
	reference := "pb-0123456789abcdef0123456789abcdef"
	transport := supportReferenceTransport{base: supportRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get(supportref.Header); got != reference {
			t.Fatalf("reference=%q", got)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	request, _ := http.NewRequestWithContext(supportref.WithContext(context.Background(), reference), http.MethodGet, "http://paperboat.local/v1/snapshot", nil)
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatal(err)
	}
}
