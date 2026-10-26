package httpapi

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// compressMinBytes is the smallest JSON body worth compressing; below it gzip's framing outweighs the saving.
const compressMinBytes = 1024

// uncompressedPaths carry bearer relay grants next to request-influenced fields; compressing them would let a
// length side channel (BREACH) recover the grant, so they always go out as identity.
var uncompressedPaths = map[string]bool{
	"/v1/peer-network/register":         true,
	"/v1/peer-network/config":           true,
	"/v1/machine-peer-network/register": true,
	"/v1/machine-peer-network/config":   true,
}

var gzipWriters = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return w
}}

// compressResponses gzips JSON responses for machine and CLI clients (PR-03: an idle machine's API bytes; the
// machine list is 250 KB as JSON and 16 KB gzipped). Only requests that carry their own credential
// (Authorization or a machine proof) and no cookie qualify: BREACH needs a victim browser that sends an ambient credential with a
// request the attacker shapes, so console (cookie) responses stay identity, as do the peer-network config paths.
// Prior art: headscale hscontrol/poll.go and tailscale control/controlclient compress the map response on request.
func compressResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !compressible(r) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w}
		defer cw.finish()
		next.ServeHTTP(cw, r)
	})
}

func compressible(r *http.Request) bool {
	if r.Method == http.MethodHead || uncompressedPaths[r.URL.Path] {
		return false
	}
	if r.Header.Get("Cookie") != "" || !carriesOwnCredential(r.Header) {
		return false
	}
	return acceptsGzip(r.Header.Values("Accept-Encoding"))
}

// carriesOwnCredential reports a credential the client attached itself (CLI and API tokens, machine proofs),
// which a browser never adds to a cross-site request on its own.
func carriesOwnCredential(h http.Header) bool {
	return h.Get("Authorization") != "" || h.Get("X-Paperboat-Machine-Proof") != "" || h.Get("X-Paperboat-Machine-Identity") != ""
}

func acceptsGzip(values []string) bool {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
				continue
			}
			q, ok := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q=")
			if !ok {
				return true
			}
			weight, err := strconv.ParseFloat(q, 64)
			return err == nil && weight > 0
		}
	}
	return false
}

// compressWriter holds the start of the body until it knows whether to compress: the response must be JSON,
// not already encoded, carry a body (not 204/304), and reach compressMinBytes. Streaming handlers that flush
// early are compressed from that point on if they qualified, or passed through untouched.
type compressWriter struct {
	http.ResponseWriter
	status  int
	decided bool
	gz      *gzip.Writer
	pending bytes.Buffer
}

func (w *compressWriter) WriteHeader(status int) {
	if w.decided || w.status != 0 {
		return // net/http ignores a second WriteHeader too
	}
	if status < 200 {
		w.ResponseWriter.WriteHeader(status) // informational: the final header follows
		return
	}
	w.status = status
	if !w.eligibleHeaders() {
		_ = w.decide(false)
	}
}

func (w *compressWriter) eligibleHeaders() bool {
	h := w.Header()
	if w.status == http.StatusNoContent || w.status == http.StatusNotModified || h.Get("Content-Encoding") != "" {
		return false
	}
	mediaType, _, _ := strings.Cut(h.Get("Content-Type"), ";")
	mediaType = strings.TrimSpace(strings.ToLower(mediaType))
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func (w *compressWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.decided {
		if w.gz != nil {
			return w.gz.Write(p)
		}
		return w.ResponseWriter.Write(p)
	}
	w.pending.Write(p)
	if w.pending.Len() >= compressMinBytes {
		if err := w.decide(true); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// decide sends the header and anything pending, compressed when compress is set and the headers qualify.
func (w *compressWriter) decide(compress bool) error {
	if w.decided {
		return nil
	}
	w.decided = true
	h := w.Header()
	h.Add("Vary", "Accept-Encoding")
	if compress && w.eligibleHeaders() {
		h.Set("Content-Encoding", "gzip")
		h.Del("Content-Length")
		gz := gzipWriters.Get().(*gzip.Writer)
		gz.Reset(w.ResponseWriter)
		w.gz = gz
	}
	w.ResponseWriter.WriteHeader(cmpStatus(w.status))
	if w.pending.Len() == 0 {
		return nil
	}
	var err error
	if w.gz != nil {
		_, err = w.gz.Write(w.pending.Bytes())
	} else {
		_, err = w.ResponseWriter.Write(w.pending.Bytes())
	}
	w.pending.Reset()
	return err
}

func cmpStatus(status int) int {
	if status == 0 {
		return http.StatusOK
	}
	return status
}

// finish ends the response: a body still under compressMinBytes goes out as identity.
func (w *compressWriter) finish() {
	if w.status == 0 && w.pending.Len() == 0 && !w.decided {
		return // the handler wrote nothing; net/http sends its default 200
	}
	_ = w.decide(false)
	if w.gz != nil {
		_ = w.gz.Close()
		w.gz.Reset(nil)
		gzipWriters.Put(w.gz)
		w.gz = nil
	}
}

func (w *compressWriter) Flush() {
	if !w.decided {
		_ = w.decide(w.eligibleHeaders())
	}
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *compressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.decided {
		return nil, nil, errors.New("hijack after the response started")
	}
	// ResponseController follows Unwrap through the wrappers below (statusRecorder has no Hijack of its own).
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.decided = true // the connection is the handler's now; finish must not write
	}
	return conn, rw, err
}

func (w *compressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
