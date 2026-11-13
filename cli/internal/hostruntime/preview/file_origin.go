package preview

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/inbox"
	"github.com/pinksaucepasta/paperboat/internal/transfername"
)

// ErrSharedFileUnavailable means a file preview's Inbox file is gone or the Inbox cannot be read.
var ErrSharedFileUnavailable = errors.New("shared inbox file is unavailable")

// sharedFileSandbox is the policy for documents that could run script (HTML, SVG): an opaque origin with
// no script, forms or plugins, loading only inline styles and images, as raw.githubusercontent.com does
// for user files ("default-src 'none'; style-src 'unsafe-inline'; sandbox").
const sharedFileSandbox = "sandbox; default-src 'none'; style-src 'unsafe-inline' https:; img-src data: https:; media-src data: https:; font-src data: https:"

// sharedFileTypes are the files that open in the browser; anything else downloads.
var sharedFileTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".md": "text/plain; charset=utf-8", ".markdown": "text/plain; charset=utf-8", ".txt": "text/plain; charset=utf-8",
	".log": "text/plain; charset=utf-8", ".csv": "text/plain; charset=utf-8", ".json": "application/json",
	".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".avif": "image/avif", ".svg": "image/svg+xml",
}

// dialFileOrigin is the origin of a "file" preview: an in-process HTTP server for the one Inbox file
// whose id is the target address, reached over a pipe, so every carrier path (public, inspected, browser)
// forwards to it like any HTTP origin. The id is resolved in the Inbox folder, never as a path.
func dialFileOrigin(inboxPath func() (string, error), id string) (net.Conn, error) {
	if inboxPath == nil {
		return nil, ErrSharedFileUnavailable
	}
	root, err := inboxPath()
	if err != nil || root == "" {
		return nil, errors.Join(ErrSharedFileUnavailable, err)
	}
	// A missing file is not a dial failure: the edge would answer 502; sharedFileHandler answers 404.
	client, server := net.Pipe()
	listener := &pipeListener{conn: &closeNotifyConn{Conn: server, closed: make(chan struct{})}}
	go func() {
		_ = (&http.Server{Handler: sharedFileHandler(root, id), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second}).Serve(listener)
	}()
	return client, nil
}

// sharedFileHandler serves the file at "/" only, as Tailscale's serve does for a single file
// (ipn/ipnlocal/serve.go serveFileOrDirectory), with ranges and conditional requests from ServeContent.
func sharedFileHandler(root, id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		handle, file, err := inbox.Open(root, id)
		if err != nil {
			http.Error(w, "This shared file is no longer available.", http.StatusNotFound)
			return
		}
		defer handle.Close()
		header := w.Header()
		contentType, inline := sharedFileTypes[strings.ToLower(filepath.Ext(file.Name))]
		disposition := "inline"
		if !inline {
			contentType, disposition = "application/octet-stream", "attachment"
		}
		header.Set("Content-Type", contentType)
		header.Set("Content-Disposition", disposition+"; filename*=UTF-8''"+url.PathEscape(transfername.Portable(file.Name)))
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cache-Control", "private, no-cache")
		if strings.HasPrefix(contentType, "text/html") || contentType == "image/svg+xml" {
			header.Set("Content-Security-Policy", sharedFileSandbox)
		}
		// Exactly the size Open saw: a file that grows while it is served is cut off, not extended.
		http.ServeContent(w, r, file.Name, file.ReceivedAt, io.NewSectionReader(handle, 0, file.Size))
	})
}

// pipeListener hands its one connection to http.Server, then blocks until that connection closes.
type pipeListener struct {
	once sync.Once
	conn *closeNotifyConn
}

func (l *pipeListener) Accept() (net.Conn, error) {
	var first net.Conn
	l.once.Do(func() { first = l.conn })
	if first != nil {
		return first, nil
	}
	<-l.conn.closed
	return nil, net.ErrClosed
}

func (l *pipeListener) Close() error   { return nil }
func (l *pipeListener) Addr() net.Addr { return l.conn.LocalAddr() }

type closeNotifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *closeNotifyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
