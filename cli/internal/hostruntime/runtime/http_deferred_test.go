package runtime

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// A deferred Start leaves the address alone however long the components around it take; Bind takes it at once.
func TestHTTPServiceDeferredStartBindsOnlyOnBind(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := NewHTTPService(HTTPConfig{Address: address, Handler: http.NotFoundHandler()})
	if err != nil {
		t.Fatal(err)
	}
	service.DeferBind()
	ctx := context.Background()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	held, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("a deferred Start took the address: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := service.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Bind took %v on a free address", took)
	}
	if connection, err := net.Dial("tcp", address); err != nil {
		t.Fatalf("nothing serves after Bind: %v", err)
	} else {
		_ = connection.Close()
	}
	if err := service.Bind(ctx); err == nil {
		t.Fatal("a second Bind succeeded")
	}
	shutdown, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := service.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPServiceBindWithoutDeferredStartIsRefused(t *testing.T) {
	service, err := NewHTTPService(HTTPConfig{Address: "127.0.0.1:0", Handler: http.NotFoundHandler()})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Bind(context.Background()); err == nil {
		t.Fatal("Bind without DeferBind succeeded")
	}
}
