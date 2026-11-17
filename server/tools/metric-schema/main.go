package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/httpapi"
	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/metrics/schema"
)

func main() { schema.Main(buildMetrics, verifyHandler) }

// buildMetrics is every metric the control plane exposes: the platform counters
// and control-plane diagnostics (flat names) and the typed telemetry catalog.
func buildMetrics() ([]schema.Metric, error) {
	names := append(serverdiagnostics.MetricNames(), controlplane.DiagnosticMetricNames()...)
	metrics := make([]schema.Metric, 0, len(names))
	for _, name := range names {
		kind := "gauge"
		if strings.HasSuffix(name, "_total") {
			kind = "counter"
		}
		metrics = append(metrics, schema.Metric{Name: name, Kind: kind})
	}
	return append(metrics, schema.FromDescriptors(telemetry.MetricDescriptors())...), nil
}

// verifyHandler checks that the localhost /metrics JSON carries exactly the
// platform counters.
func verifyHandler(schema.Document) error {
	router := httpapi.NewRouter(httpapi.Options{Config: config.Default(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		return fmt.Errorf("status %d", recorder.Code)
	}
	var response struct {
		Data map[string]int64 `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		return err
	}
	want := serverdiagnostics.MetricNames()
	got := make([]string, 0, len(response.Data))
	for name := range response.Data {
		got = append(got, name)
	}
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		return fmt.Errorf("process metrics differ: got %v want %v", got, want)
	}
	return nil
}
