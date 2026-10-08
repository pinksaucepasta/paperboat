package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestFleetInventoryExhaustsPagesAndRejectsPartialResults(t *testing.T) {
	for _, failure := range []string{"", "duplicate", "http", "total", "counts", "state", "missing-pagination"} {
		t.Run(failure, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/v1/machines/update-summary" || r.URL.Query().Get("limit") != "200" || r.URL.Query().Get("offset") != strconv.Itoa((requests-1)*200) {
					t.Errorf("summary continuation changed: %s", r.URL.String())
				}
				if requests == 2 && failure == "http" {
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprint(w, `{"error":{"code":"unavailable","message":"fixture unavailable"}}`)
					return
				}
				offset := (requests - 1) * 200
				end := min(offset+200, 401)
				items := []map[string]any{}
				for i := offset; i < end; i++ {
					id := fmt.Sprintf("machine_%d", i)
					state := "healthy"
					if requests == 2 && i == offset && failure == "duplicate" {
						id = "machine_0"
					}
					if requests == 2 && failure == "state" {
						state = "unknown"
					}
					items = append(items, map[string]any{"machine_id": id, "alias": fmt.Sprintf("Machine %d", i), "state": state, "online": true})
				}
				page := MachineUpdateSummaryPage{Items: items, Counts: map[string]uint64{"healthy": 401}, Pagination: Pagination{Limit: 200, Offset: offset, Total: 401}}
				if requests == 2 && failure == "total" {
					page.Pagination.Total = 402
					page.Counts["healthy"] = 402
				}
				if requests == 2 && failure == "counts" {
					page.Counts["healthy"] = 400
				}
				if end < page.Pagination.Total {
					page.Pagination.NextOffset = &end
				}
				if requests == 2 && failure == "missing-pagination" {
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items, "counts": page.Counts}})
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"data": page})
			}))
			defer server.Close()
			result, err := New(server.URL, config.Credential{AccessToken: "fixture-token"}, server.Client()).MachineUpdateSummary(context.Background())
			if failure != "" {
				if err == nil || result != nil || requests != 2 {
					t.Fatalf("partial result became success: requests=%d result=%v err=%v", requests, result, err)
				}
				if failure != "http" && !errors.Is(err, ErrFleetInventoryInvalid) {
					t.Fatalf("missing typed invalid inventory cause: %v", err)
				}
				return
			}
			if err != nil || requests != 3 || len(result["items"].([]map[string]any)) != 401 || result["counts"].(map[string]uint64)["healthy"] != 401 {
				t.Fatalf("fleet truncated: requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestFleetInventoryPagePreservesFiltersDefaultAndMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "50" || r.URL.Query().Get("offset") != "17" || r.URL.Query().Get("q") != "fixture_% & exact?" || r.URL.Query().Get("state") != "not_reporting" {
			t.Errorf("summary filters changed: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"data":{"items":[],"counts":{"not_reporting":0},"pagination":{"limit":50,"offset":17,"total":0,"next_offset":null}}}`)
	}))
	defer server.Close()
	page, err := New(server.URL, config.Credential{AccessToken: "fixture-token"}, server.Client()).MachineUpdateSummaryPage(context.Background(), 0, 17, "fixture_% & exact?", "not_reporting")
	if err != nil || page.Pagination.Limit != 50 || page.Pagination.Offset != 17 || page.Pagination.NextOffset != nil || page.Counts["not_reporting"] != 0 {
		t.Fatalf("page metadata lost: %+v err=%v", page, err)
	}
}

// Closing the fully consumed first response cancels between pages, rather than
// replacing a failed request with an artificial successful empty inventory.
type fleetCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b fleetCancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

func TestFleetInventoryCancellationStopsAfterCompletedFirstPage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		items := []map[string]any{}
		for i := 0; i < 200; i++ {
			items = append(items, map[string]any{"machine_id": fmt.Sprintf("machine_%d", i), "state": "healthy"})
		}
		next := 200
		data, _ := json.Marshal(map[string]any{"data": MachineUpdateSummaryPage{Items: items, Counts: map[string]uint64{"healthy": 201}, Pagination: Pagination{Limit: 200, Offset: 0, Total: 201, NextOffset: &next}}})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: fleetCancelBody{ReadCloser: io.NopCloser(strings.NewReader(string(data))), cancel: cancel}, Request: r}, nil
	})}
	result, err := New("https://fixture.invalid", config.Credential{AccessToken: "fixture-token"}, httpClient).MachineUpdateSummary(ctx)
	if !errors.Is(err, context.Canceled) || result != nil || requests != 1 {
		t.Fatalf("cancellation exposed partial fleet: requests=%d result=%v err=%v", requests, result, err)
	}
}
