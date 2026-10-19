package edgehttp

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
)

func TestReadFinalHTTPResponseSkipsInterimResponses(t *testing.T) {
	stream := "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: </a.css>; rel=preload\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	var hinted []int
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
		hinted = append(hinted, code)
		return nil
	}}
	request, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodPost, "http://origin.invalid/", nil)
	response, err := readFinalHTTPResponse(bufio.NewReader(strings.NewReader(stream)), request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("final response = %d %q", response.StatusCode, body)
	}
	if len(hinted) != 1 || hinted[0] != 103 {
		t.Fatalf("interim responses passed on = %v, want only 103 (100 Continue is swallowed)", hinted)
	}
}

func TestReadFinalHTTPResponseKeepsSwitchingProtocolsFinalAndBoundsInterims(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "http://origin.invalid/", nil)
	upgrade := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
	response, err := readFinalHTTPResponse(bufio.NewReader(strings.NewReader(upgrade)), request)
	if err != nil || response.StatusCode != 101 {
		t.Fatalf("upgrade = %v %v", response, err)
	}
	endless := strings.Repeat("HTTP/1.1 100 Continue\r\n\r\n", maximumInterimResponses+2)
	if _, err := readFinalHTTPResponse(bufio.NewReader(strings.NewReader(endless)), request); err == nil {
		t.Fatal("an origin that never sends a final response was accepted")
	}
}
