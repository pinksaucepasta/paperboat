package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

type fakeInboxObservationRepository struct {
	recorded *usermachines.InboxObservation
	err      error
}

func (f *fakeInboxObservationRepository) RecordInboxObservation(_ context.Context, _, _ string, observation usermachines.InboxObservation) error {
	f.recorded = &observation
	return f.err
}

func inboxHeartbeat(t *testing.T, sink any, inbox string) (int, string) {
	t.Helper()
	body := `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-06T12:00:01Z","inbox":` + inbox + `}`
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	addRuntimeProof(request)
	recorder := httptest.NewRecorder()
	runtimeObservation(&fakeRuntimeObservationRepository{}, &fakeRuntimeIdentity{}, 10, sink).ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func TestRuntimeObservationRecordsInboxListing(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Second).Format(time.RFC3339)
	inbox := `{"observed_at":"` + now + `","total":1,"items":[{"id":"abc","name":"a.txt","size":3,"received_at":"` + now + `"}]}`
	sink := &fakeInboxObservationRepository{}
	status, body := inboxHeartbeat(t, sink, inbox)
	if status != http.StatusAccepted || sink.recorded == nil || sink.recorded.Total != 1 || sink.recorded.Items[0].Name != "a.txt" || strings.Contains(body, "auxiliary_rejections") {
		t.Fatalf("status=%d recorded=%+v body=%s", status, sink.recorded, body)
	}
}

func TestRuntimeObservationKeepsPresenceWhenInboxListingIsRejected(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Second).Format(time.RFC3339)
	valid := `{"observed_at":"` + now + `","total":0,"items":[]}`
	for name, tc := range map[string]struct {
		sink  any
		inbox string
		code  string
	}{
		"invalid":     {&fakeInboxObservationRepository{}, `{"observed_at":"` + now + `","total":0,"items":[{"id":"x","name":"../x","size":1,"received_at":"` + now + `"}]}`, "inbox_observation_invalid"},
		"stale":       {&fakeInboxObservationRepository{err: usermachines.ErrInboxObservationStale}, valid, "inbox_observation_stale"},
		"unavailable": {&fakeInboxObservationRepository{err: context.DeadlineExceeded}, valid, "inbox_observation_unavailable"},
		"no sink":     {nil, valid, "inbox_observation_unavailable"},
	} {
		status, body := inboxHeartbeat(t, tc.sink, tc.inbox)
		if status != http.StatusAccepted || !strings.Contains(body, `"code":"`+tc.code+`"`) {
			t.Errorf("%s: status=%d body=%s", name, status, body)
		}
	}
}
