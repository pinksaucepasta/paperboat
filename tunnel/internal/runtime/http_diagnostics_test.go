package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
)

func TestHTTPDiagnosticsPanicAbortAndRecovery(t *testing.T) {
	const reference = "support_10000000-0000-4000-8000-000000000001"
	if os.Getenv("PB_HTTP_DIAGNOSTIC_HELPER") == "1" {
		reporter, err := reporting.New("paperboat-tunnel")
		if err != nil {
			t.Fatal(err)
		}
		defer reporter.Close()
		ctx := reporting.WithSupportReference(context.Background(), reference)
		var mode atomic.Int32
		handler := httpDiagnosticHandler(ctx, reporter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch mode.Load() {
			case 0:
				panic("PRIVATE_PANIC_PAYLOAD")
			case 2:
				panic(http.ErrAbortHandler)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		server := httptest.NewUnstartedServer(handler)
		server.Config.ErrorLog = httpDiagnosticLogger(ctx, reporter)
		server.Start()
		defer server.Close()
		transport := &http.Transport{}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: time.Second}
		if response, err := client.Get(server.URL + "/PRIVATE_PATH"); err == nil {
			response.Body.Close()
			t.Fatal("panic produced successful response")
		}
		mode.Store(1)
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatal("HTTP recovery failed")
		}
		mode.Store(2)
		if response, err := client.Get(server.URL); err == nil {
			response.Body.Close()
			t.Fatal("AbortHandler produced successful response")
		}
		server.Config.ErrorLog.Printf("TLS failure from PRIVATE_IP: PRIVATE_CREDENTIAL")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHTTPDiagnosticsPanicAbortAndRecovery$")
	command.Env = append(os.Environ(), "PB_HTTP_DIAGNOSTIC_HELPER=1", "PAPERBOAT_SENTRY_ENABLED=false")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("HTTP diagnostic helper failed: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "PRIVATE_") {
		t.Fatal("HTTP diagnostic leaked private data")
	}
	var faults []reporting.Fault
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var fault reporting.Fault
		if err := json.Unmarshal([]byte(line), &fault); err != nil {
			t.Fatal("invalid HTTP diagnostic")
		}
		faults = append(faults, fault)
	}
	if len(faults) != 2 {
		t.Fatalf("faults=%d, want exactly panic and finite fallback; AbortHandler must be silent", len(faults))
	}
	if faults[0].Code != "process_panic" || faults[0].Cause != "process_panic" || faults[1].Code != "http_server_failed" {
		t.Fatalf("HTTP diagnostics lost classification: %+v", faults)
	}
	for _, fault := range faults {
		if fault.SupportReference != reference || fault.CorrelationID != reference || fault.SourceFile == "" {
			t.Fatal("HTTP diagnostic lost process correlation/source")
		}
	}
}
