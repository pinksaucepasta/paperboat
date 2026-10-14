package inspectorapi

import (
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCapturePageBoundsEncodedBodiesAndContinues(t *testing.T) {
	now := time.Now().UTC()
	a := &fakeAuthorizer{decisions: map[string]Decision{"grant\x00tunnel\x00tun_01\x00rte_01\x00inspect": testDecision("owner_01", "tunnel", "tun_01", "rte_01", now)}}
	s, store, _ := testService(t, a)
	_ = store.SetPolicy("rte_01", inspector.ResourcePolicy{Enabled: true, CaptureRequestBody: true, CaptureResponseBody: true})
	body := []byte(`{"value":"` + strings.Repeat(`\"`, 30000) + `"}`)
	for i := 0; i < 12; i++ {
		p, err := store.TryBegin("rte_01")
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Finish(p, inspector.CompletedCapture{Method: "POST", RawURL: "http://example.test/", RequestHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseHeaders: http.Header{"Content-Type": {"application/json"}}, RequestBody: body, ResponseBody: body, ResourceGeneration: 3, RouteGeneration: 3, TargetGeneration: 3})
		if err != nil {
			t.Fatal(err)
		}
	}
	cursor := ""
	seen := map[string]bool{}
	for i := 0; i < 12; i++ {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, testRequest(t, http.MethodGet, "/v1/inspector/records?kind=tunnel&resource=tun_01&route=rte_01&limit=100&cursor="+url.QueryEscape(cursor), nil, "grant"))
		if w.Code != http.StatusOK || w.Body.Len() > inspector.RetrievalMaxBytes {
			t.Fatalf("invalid page: status=%d bytes=%d", w.Code, w.Body.Len())
		}
		var page struct {
			Records    []recordWire `json:"records"`
			NextCursor string       `json:"next_cursor"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Records {
			if seen[r.ID] {
				t.Fatal("duplicate record")
			}
			seen[r.ID] = true
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 12 {
		t.Fatalf("lost records: got %d", len(seen))
	}
}
