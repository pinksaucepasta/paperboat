package recordingcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordings"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
)

func TestUploadURLsAnswersAreDecodedAndNoBucketIsNamed(t *testing.T) {
	var asked teambucket.UploadRequest
	fetcher, proofs := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == teambucket.UploadURLsPath("rec_1") {
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&asked) != nil {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": teambucket.UploadURLs{Uploads: []teambucket.Authority{{URL: "https://bucket.example.test/a", Headers: map[string]string{"h": "v"}}}}})
			return
		}
		w.WriteHeader(http.StatusConflict)
	})
	got, err := fetcher.UploadURLs(context.Background(), "rec_1", []teambucket.Segment{{Index: 4, Bytes: 9}})
	if err != nil || len(got) != 1 || got[0].Headers["h"] != "v" || len(asked.Segments) != 1 || asked.Segments[0].Index != 4 || len(proofs.bodies) != 1 || len(proofs.bodies[0]) == 0 {
		t.Fatalf("uploads %+v asked %+v proof bodies %d, %v", got, asked, len(proofs.bodies), err)
	}
	if _, err := fetcher.UploadURLs(context.Background(), "rec_2", nil); !errors.Is(err, ErrNoBucket) {
		t.Fatalf("a team without a bucket = %v", err)
	}
}

type fakeArchive struct {
	asked    []int // segments per UploadURLs call
	archived []int
	noBucket bool
}

func (a *fakeArchive) UploadURLs(_ context.Context, id string, segments []teambucket.Segment) ([]teambucket.Authority, error) {
	if a.noBucket {
		return nil, ErrNoBucket
	}
	a.asked = append(a.asked, len(segments))
	out := make([]teambucket.Authority, len(segments))
	for i, s := range segments {
		out[i] = teambucket.Authority{URL: fmt.Sprintf("https://bucket.example.test/%s/%d", id, s.Index)}
	}
	return out, nil
}
func (a *fakeArchive) Archived(_ context.Context, _ string, n int) error {
	a.archived = append(a.archived, n)
	return nil
}

// Closed segments reach the bucket in order, at most a batch per request, the position survives, a failed upload
// resumes where it stopped with a growing wait, and a team without a bucket is left alone for a while.
func TestArchiveCopiesClosedSegmentsInOrderAndResumes(t *testing.T) {
	sessions := &fakeSessions{snapshots: map[string]session.Snapshot{}, metas: []recordings.Meta{{ID: "rec_a", Segments: 20}, {ID: "rec_b", Segments: 2}}}
	controller, _ := testController(t, policyWith(1, newKey(t, "team_1", 1, nil).key), sessions)
	archive := &fakeArchive{}
	var puts []string
	failAt := ""
	controller.config.Archive = archive
	controller.config.Put = func(_ context.Context, u teambucket.Authority, data []byte) error {
		if u.URL == failAt {
			return errors.New("bucket down")
		}
		puts = append(puts, u.URL)
		return nil
	}
	now := time.Now()
	controller.config.Now = func() time.Time { return now }

	failAt = "https://bucket.example.test/rec_a/18" // rec_b is the newer recording; the oldest (last) is uploaded first
	sessions.metas = []recordings.Meta{{ID: "rec_b", Segments: 2}, {ID: "rec_a", Segments: 20}}
	controller.archive(context.Background())
	if controller.archivedCount("rec_a") != 16 || controller.archivedCount("rec_b") != 0 || len(puts) != 18 {
		t.Fatalf("after a failure: a=%d b=%d puts=%d", controller.archivedCount("rec_a"), controller.archivedCount("rec_b"), len(puts))
	}
	if !controller.config.Now().Before(controller.archiveNext) || controller.archiveFailures != 1 {
		t.Fatalf("no backoff: next %v failures %d", controller.archiveNext, controller.archiveFailures)
	}
	controller.archive(context.Background()) // inside the wait: nothing happens
	if len(puts) != 18 {
		t.Fatal("retried before its wait")
	}
	now = now.Add(time.Hour)
	failAt = ""
	puts = nil
	controller.archive(context.Background())
	if controller.archivedCount("rec_a") != 20 || controller.archivedCount("rec_b") != 2 || len(puts) != 6 || puts[0] != "https://bucket.example.test/rec_a/16" {
		t.Fatalf("resume: a=%d b=%d puts=%v", controller.archivedCount("rec_a"), controller.archivedCount("rec_b"), puts)
	}
	if fmt.Sprint(archive.archived) != "[16 20 2]" || controller.archiveFailures != 0 {
		t.Fatalf("archived reports %v failures %d", archive.archived, controller.archiveFailures)
	}
	// The position is in the ledger file: a new controller on the same file does not upload again.
	reopened, err := openLedger(controller.ledger.path)
	if err != nil || reopened.Archived["rec_a"] != 20 {
		t.Fatalf("ledger %+v %v", reopened, err)
	}

	// No bucket: asked once, then left alone for the wait.
	sessions.metas = append(sessions.metas, recordings.Meta{ID: "rec_c", Segments: 1})
	archive.noBucket = true
	controller.archive(context.Background())
	if got := controller.archiveNext.Sub(now); got < noBucketWait-time.Second {
		t.Fatalf("no-bucket wait %v", got)
	}
}
