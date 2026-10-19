package edgehttp

import (
	"bufio"
	"errors"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
)

const maximumInterimResponses = 8

var errTooManyInterimResponses = errors.New("origin sent too many interim responses")

// readFinalHTTPResponse reads the response to a request written onto an origin stream. An origin's
// HTTP server answers a request that carried Expect: 100-continue with an interim "100 Continue"
// before the real response; that is not the answer, and treating it as one cut the real response off.
// Interim responses are skipped; informational ones other than 100 are passed on through the
// request's trace hook (which is how ReverseProxy relays them), 101 Switching Protocols is final.
func readFinalHTTPResponse(reader *bufio.Reader, request *http.Request) (*http.Response, error) {
	trace := httptrace.ContextClientTrace(request.Context())
	for interim := 0; interim <= maximumInterimResponses; interim++ {
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode < 100 || response.StatusCode >= 200 || response.StatusCode == http.StatusSwitchingProtocols {
			return response, nil
		}
		if response.StatusCode != http.StatusContinue && trace != nil && trace.Got1xxResponse != nil {
			if err := trace.Got1xxResponse(response.StatusCode, textproto.MIMEHeader(response.Header)); err != nil {
				return nil, err
			}
		}
	}
	return nil, errTooManyInterimResponses
}
