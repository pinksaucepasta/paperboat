package app

import (
	"net/http"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

func TestNewHTTPServerBoundsConnectionsWithoutWriteTimeout(t *testing.T) {
	cfg := config.Default().HTTP
	server := newHTTPServer(cfg, http.NotFoundHandler())
	if server.Addr != cfg.Address || server.ReadHeaderTimeout != cfg.ReadHeaderTimeout || server.ReadTimeout != cfg.RequestTimeout {
		t.Fatalf("addr = %q, read header = %s, read = %s", server.Addr, server.ReadHeaderTimeout, server.ReadTimeout)
	}
	if server.IdleTimeout != cfg.IdleTimeout || server.IdleTimeout <= 0 {
		t.Fatalf("idle timeout = %s, want %s", server.IdleTimeout, cfg.IdleTimeout)
	}
	if server.MaxHeaderBytes != cfg.MaxHeaderBytes || server.MaxHeaderBytes <= 0 {
		t.Fatalf("max header bytes = %d, want %d", server.MaxHeaderBytes, cfg.MaxHeaderBytes)
	}
	if server.WriteTimeout != 0 {
		t.Fatalf("write timeout = %s, want unset so streaming routes are not cut", server.WriteTimeout)
	}
}
