package recordings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teambucket"
	"github.com/pinksaucepasta/paperboat/shared/recording"
	shared "github.com/pinksaucepasta/paperboat/shared/teambucket"
)

// fakeBucket is a team bucket in memory: it presigns URLs naming the key and records what is deleted.
type fakeBucket struct {
	use       teambucket.Use // "" when the team has no bucket
	prefix    string
	deleted   []string
	deleteErr error
}

func (b *fakeBucket) StoreFor(_ context.Context, _ *db.Tx, _ string, use teambucket.Use) (teambucket.ObjectStore, error) {
	switch {
	case b.use == "" && b.prefix == "none":
		return nil, teambucket.ErrNotFound
	case use != teambucket.UseAny && use != b.use:
		return nil, teambucket.ErrUseOff
	}
	return b, nil
}
func (b *fakeBucket) Config() shared.Config { return shared.Config{Prefix: b.prefix} }
func (b *fakeBucket) PresignPut(_ context.Context, key string, size int64, digest string, _ time.Duration) (shared.Authority, error) {
	return shared.Authority{URL: "https://bucket.example.test/" + key + "?put", Headers: map[string]string{"x-amz-checksum-sha256": digest}}, nil
}
func (b *fakeBucket) PresignGet(_ context.Context, key string, _ time.Duration) (shared.Authority, error) {
	return shared.Authority{URL: "https://bucket.example.test/" + key + "?get"}, nil
}
func (b *fakeBucket) Delete(_ context.Context, key string) error {
	if b.deleteErr != nil {
		return b.deleteErr
	}
	b.deleted = append(b.deleted, key)
	return nil
}

// The archive in the team bucket: upload URLs only for the recording's own machine and only when the team chose the
// bucket for recordings, a monotonic archived count, a bucket replay that needs no machine and is audited first, and
// retention that deletes the segments and forgets them.
func TestPostgresRecordingsArchive(t *testing.T) {
	f := newFixture(t)
	f.paid()
	if err := f.policy(`{"rules": [{"src": ["user:` + f.member + `@example.test"], "dst": ["machine:prod"], "allow": ["recording_view"]}]}`); err != nil {
		t.Fatal(err)
	}
	k1, _ := newKey(t, f.team, 1, nil)
	if _, err := f.s.CreateKey(f.ctx, f.owner, f.team, recording.KeyCreate{Key: k1, Grants: []recording.Grant{f.grant(f.member)}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	apply := func(machine, kind string, r recording.Report) {
		t.Helper()
		if err := f.store.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) error { return ApplyReportTx(ctx, tx, machine, kind, r) }); err != nil {
			t.Fatal(err)
		}
	}
	apply(f.prod, recording.KindStarted, recording.Report{RecordingID: recID, SessionID: "sess_1", Trigger: recording.TriggerRule, KeyGeneration: 1, Accounts: []string{f.member}, StartedAt: now})
	apply(f.prod, recording.KindStopped, recording.Report{RecordingID: recID, Reason: recording.ReasonSessionEnded, Bytes: 4096, Segments: 3, EndedAt: now})

	digest := strings.Repeat("ab", 32)
	request := shared.UploadRequest{Segments: []shared.Segment{{Index: 0, Bytes: 100, SHA256: digest}, {Index: 1, Bytes: 200, SHA256: digest}}}

	// No bucket, or a bucket chosen for files only: nothing to upload to.
	for _, b := range []*fakeBucket{{prefix: "none"}, {use: teambucket.UseFiles}} {
		f.s.SetBuckets(b)
		if _, err := f.s.UploadURLs(f.ctx, f.prod, recID, request); !errors.Is(err, ErrNoBucket) {
			t.Fatalf("upload without a recordings bucket = %v", err)
		}
	}
	bucket := &fakeBucket{use: teambucket.UseRecordings, prefix: "paperboat"}
	f.s.SetBuckets(bucket)
	if _, err := f.s.UploadURLs(f.ctx, f.dev, recID, request); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another machine's upload = %v", err)
	}
	if _, err := f.s.UploadURLs(f.ctx, f.prod, recID, shared.UploadRequest{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty upload request = %v", err)
	}
	urls, err := f.s.UploadURLs(f.ctx, f.prod, recID, request)
	if err != nil || len(urls.Uploads) != 2 || !strings.HasSuffix(strings.Split(urls.Uploads[1].URL, "?")[0], "paperboat/v1/recordings/"+recID+"/000001.seg") || urls.Uploads[0].Headers["x-amz-checksum-sha256"] != digest {
		t.Fatalf("uploads = %+v, %v", urls, err)
	}

	// The archived count only grows, and only its own machine moves it.
	if err := f.s.Archived(f.ctx, f.dev, recID, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another machine's report = %v", err)
	}
	for _, n := range []int{2, 1} {
		if err := f.s.Archived(f.ctx, f.prod, recID, n); err != nil {
			t.Fatal(err)
		}
	}
	list, err := f.s.List(f.ctx, f.owner, f.team, Filter{})
	if err != nil || len(list.Recordings) != 1 || list.Recordings[0].ArchivedSegments != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}

	// Bucket replay: a viewer with the rule, audited, one download per archived segment, not final (3 of 2 here).
	if _, err := f.s.Replay(f.ctx, f.bo, recID, recording.ReplayRequest{Source: recording.SourceBucket}); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-viewer bucket replay = %v", err)
	}
	if _, err := f.s.Replay(f.ctx, f.member, recID, recording.ReplayRequest{Source: recording.SourceBucket, CLIClientSessionID: "cli_" + f.member}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bucket replay with a client field = %v", err)
	}
	out, err := f.s.Replay(f.ctx, f.member, recID, recording.ReplayRequest{Source: recording.SourceBucket})
	if err != nil || out.Source != recording.SourceBucket || len(out.Segments) != 2 || out.Final || out.Ticket != "" || out.KeyGeneration != 1 || out.ViewerKey.Generation != 1 {
		t.Fatalf("bucket replay = %+v, %v", out, err)
	}
	if !strings.HasSuffix(strings.Split(out.Segments[1].URL, "?")[0], "/000001.seg") || !strings.HasSuffix(out.Segments[1].URL, "?get") {
		t.Fatalf("segment urls = %+v", out.Segments)
	}
	var audited int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='team.recording_replayed' AND resource_id=$1 AND actor_user_id=$2 AND metadata->>'source'='bucket'`, f.team, f.member).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("bucket replay audit events = %d, %v", audited, err)
	}
	if err := f.s.Archived(f.ctx, f.prod, recID, 3); err != nil {
		t.Fatal(err)
	}
	if out, err = f.s.Replay(f.ctx, f.member, recID, recording.ReplayRequest{Source: recording.SourceBucket}); err != nil || !out.Final || len(out.Segments) != 3 {
		t.Fatalf("complete archive = %+v, %v", out, err)
	}

	// The machine's copy is deleted and the machine is gone: the archive still plays, the machine path does not.
	apply(f.prod, recording.KindDeleted, recording.Report{RecordingID: recID, Reason: recording.ReasonRetention})
	f.exec(`UPDATE paperboat.user_machines SET revoked_at = now() WHERE id = $1`, f.prod)
	if _, err := f.s.Replay(f.ctx, f.member, recID, f.native()); !errors.Is(err, ErrDeleted) {
		t.Fatalf("machine replay of a deleted recording = %v", err)
	}
	if out, err = f.s.Replay(f.ctx, f.member, recID, recording.ReplayRequest{Source: recording.SourceBucket}); err != nil || len(out.Segments) != 3 {
		t.Fatalf("archive replay after the machine is gone = %+v, %v", out, err)
	}
	if _, err := f.s.Replay(f.ctx, f.member, "rec_ffffffff-ffff-4fff-8fff-ffffffffffff", recording.ReplayRequest{Source: recording.SourceBucket}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown recording = %v", err)
	}

	// Retention: past the bucket window the segments are deleted and forgotten; a failed delete keeps them for next time.
	f.exec(`UPDATE paperboat.session_recordings SET started_at = now() - interval '61 days' WHERE id = $1`, recID)
	f.s.cfg.BucketRetentionDays = 60
	bucket.deleteErr = errors.New("bucket down")
	if err := f.s.PruneArchive(f.ctx); err == nil {
		t.Fatal("a failed delete was swallowed")
	}
	if n := archivedOf(t, f, recID); n != 3 {
		t.Fatalf("archived after a failed delete = %d", n)
	}
	bucket.deleteErr = nil
	if err := f.s.PruneArchive(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(bucket.deleted) != 3 || !strings.HasSuffix(bucket.deleted[2], "paperboat/v1/recordings/"+recID+"/000002.seg") || archivedOf(t, f, recID) != 0 {
		t.Fatalf("deleted %v, archived %d", bucket.deleted, archivedOf(t, f, recID))
	}
	if _, err := f.s.Replay(f.ctx, f.member, recID, recording.ReplayRequest{Source: recording.SourceBucket}); !errors.Is(err, ErrNotArchived) {
		t.Fatalf("pruned archive replay = %v", err)
	}
}

func archivedOf(t *testing.T, f *fixture, id string) int {
	t.Helper()
	var n int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT archived_segments FROM paperboat.session_recordings WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
