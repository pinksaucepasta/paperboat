package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// recordingListServer holds total recordings and, like paperboat-server, refuses a limit above 200.
// pageStart picks where the page asked for at offset begins; a negative start fails the request.
func recordingListServer(t *testing.T, total int, pageStart func(offset int) int) (string, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/teams/acme/recordings" {
			t.Errorf("unexpected request %s", r.URL)
		}
		requests.Add(1)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		w.Header().Set("Content-Type", "application/json")
		if limit < 1 || limit > 200 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, apiFailure("invalid_request", "null"))
			return
		}
		start := pageStart(offset)
		if start < 0 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, apiFailure("forbidden", "null"))
			return
		}
		page := recording.List{Recordings: []recording.Recording{}, Total: int64(total)}
		for i := start; i < min(start+limit, total); i++ {
			page.Recordings = append(page.Recordings, recording.Recording{ID: listedRecordingID(i), MachineID: "m_1"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": page})
	}))
	t.Cleanup(server.Close)
	return server.URL, requests
}

func listedRecordingID(i int) string { return fmt.Sprintf("rec_00000000-0000-4000-8000-%012d", i) }

func TestRecordingsListReadsBoundedPages(t *testing.T) {
	for _, tc := range []struct {
		name        string
		total, want int
		requests    int32
		pageStart   func(int) int
		args        []string
	}{
		{"all", 450, 450, 3, func(offset int) int { return offset }, []string{"--all"}},
		{"all, whole pages", 400, 400, 2, func(offset int) int { return offset }, []string{"--all"}},
		{"all, none", 0, 0, 1, func(offset int) int { return offset }, []string{"--all"}},
		{"all, capped", 10100, 10000, 50, func(offset int) int { return offset }, []string{"--all"}},
		{"one page", 450, 7, 1, func(offset int) int { return offset }, []string{"--limit", "7"}},
		// A new recording between requests shifts the offsets: the repeated one is listed once.
		{"shifted", 450, 450, 3, func(offset int) int { return max(offset-1, 0) }, []string{"--all"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, requests := recordingListServer(t, tc.total, tc.pageStart)
			code, out, errOut := runPolicyCLI(t, url, append([]string{"recordings", "list", "--team", "acme", "--json"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
			}
			var envelope struct{ Data recording.List }
			if err := json.Unmarshal([]byte(out), &envelope); err != nil {
				t.Fatal(err)
			}
			got := envelope.Data
			if got.Recordings == nil {
				t.Fatalf("recordings is not an array: %.300s", out)
			}
			if len(got.Recordings) != tc.want || got.Total != int64(tc.total) || requests.Load() != tc.requests {
				t.Fatalf("listed %d total %d in %d requests: %.300s", len(got.Recordings), got.Total, requests.Load(), out)
			}
			for i, r := range got.Recordings {
				if r.ID != listedRecordingID(i) {
					t.Fatalf("recording %d is %s", i, r.ID)
				}
			}
		})
	}
}

func TestRecordingsListStopsOnAStallOrAFailedPage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pageStart func(int) int
		message   string
	}{
		{"offset ignored", func(int) int { return 0 }, "repeated a page of recordings"},
		{"second page refused", func(offset int) int {
			if offset > 0 {
				return -1
			}
			return 0
		}, "permission"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, requests := recordingListServer(t, 450, tc.pageStart)
			code, out, errOut := runPolicyCLI(t, url, "recordings", "list", "--team", "acme", "--all")
			if code == 0 || requests.Load() != 2 || strings.Contains(out, "rec_") || errOut+out == "" || !strings.Contains(errOut+out, tc.message) {
				t.Fatalf("code=%d requests=%d out=%.300s err=%s", code, requests.Load(), out, errOut)
			}
		})
	}
}
