package recordingcontrol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordings"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

// ledger records the last report kind queued for each recording still on the machine, plus why
// this runtime deleted the ones it pruned. Its size follows the recordings directory, which the
// disk quota bounds.
type ledger struct {
	path    string
	mu      sync.Mutex
	Sent    map[string]string `json:"sent"`
	Deleted map[string]string `json:"deleted,omitempty"`
}

func openLedger(path string) (*ledger, error) {
	l := &ledger{path: path, Sent: map[string]string{}, Deleted: map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the recording report ledger: %w", err)
	}
	if json.Unmarshal(data, l) != nil || l.Sent == nil {
		// Unreadable: every recording is reported again; event ids are stable, so the server
		// counts the repeats as duplicates.
		l.Sent, l.Deleted = map[string]string{}, map[string]string{}
	}
	if l.Deleted == nil {
		l.Deleted = map[string]string{}
	}
	return l, nil
}

func (l *ledger) save() error {
	encoded, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return atomicfile.Write(l.path, encoded, atomicfile.CurrentOwnerOptions(0o600))
}

// eventID is stable per recording and kind, so a report queued twice is one event on the server.
func eventID(recordingID, kind string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("paperboat-recording:"+recordingID+":"+kind)).String()
}

func started(meta recordings.Meta) wire.Report {
	return wire.Report{RecordingID: meta.ID, SessionID: meta.SessionID, Trigger: meta.Trigger, ActorAccountID: meta.StartedBy, KeyGeneration: meta.KeyGeneration, Accounts: meta.Accounts, StartedAt: meta.StartedAt}
}

func stopped(meta recordings.Meta) wire.Report {
	report := wire.Report{RecordingID: meta.ID, Reason: meta.EndReason, Accounts: meta.Accounts, Bytes: meta.Bytes, Segments: int64(meta.Segments)}
	if meta.EndReason == recordings.ReasonStopped {
		report.ActorAccountID = meta.StoppedBy
	}
	if meta.EndedAt != nil {
		report.EndedAt = *meta.EndedAt
	}
	return report
}

// queue puts one report in the audit queue. A report Paperboat would refuse (older than its window)
// is skipped, so it never blocks the ones after it.
func (c *Controller) queue(kind string, report wire.Report) error {
	if err := report.Validate(kind, c.config.Now()); err != nil {
		diagnosticlog.TryWarn("a recording report was skipped", "kind", kind)
		return nil
	}
	return c.config.Audit.Record(offlineaudit.Event{EventID: eventID(report.RecordingID, kind), Kind: kind, Recording: &report})
}

// report sends what changed since the last pass: recordings started, stopped and deleted. An open
// recording whose session no longer runs (its host process died) is closed first.
func (c *Controller) report() {
	metas, err := c.config.Sessions.Recordings()
	if err != nil {
		return
	}
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	changed := false
	present := make(map[string]bool, len(metas))
	for _, meta := range metas {
		present[meta.ID] = true
		if meta.EndedAt == nil {
			snapshot, err := c.config.Sessions.Snapshot(meta.SessionID)
			if errors.Is(err, session.ErrSessionUnknown) || err == nil && snapshot.State != session.Running && snapshot.State != session.Closing {
				if closed, err := c.config.Sessions.CloseInterruptedRecording(meta.ID); err == nil {
					meta = closed
				}
			}
		}
		sent := c.ledger.Sent[meta.ID]
		if sent == "" {
			if c.queue(wire.KindStarted, started(meta)) != nil {
				break // the audit queue is full: retry on the next pass
			}
			sent, changed = wire.KindStarted, true
		}
		if meta.EndedAt != nil && sent == wire.KindStarted {
			if c.queue(wire.KindStopped, stopped(meta)) != nil {
				c.ledger.Sent[meta.ID] = sent
				break
			}
			sent, changed = wire.KindStopped, true
		}
		c.ledger.Sent[meta.ID] = sent
	}
	for id := range c.ledger.Sent {
		if present[id] {
			continue
		}
		reason := c.ledger.Deleted[id]
		if reason == "" {
			reason = recordings.ReasonQuota // a recording deleted to make room for another
		}
		if c.queue(wire.KindDeleted, wire.Report{RecordingID: id, Reason: reason}) != nil {
			break
		}
		delete(c.ledger.Sent, id)
		delete(c.ledger.Deleted, id)
		changed = true
	}
	for id := range c.ledger.Deleted {
		if c.ledger.Sent[id] == "" {
			delete(c.ledger.Deleted, id) // pruned before it was ever reported
		}
	}
	if changed {
		if err := c.ledger.save(); err != nil {
			diagnosticlog.TryWarn("the recording report ledger could not be saved; reports may repeat")
		}
	}
}

// prune applies the policy's retention and disk quota. After a team stops recording, the last
// policy's limits keep applying to the recordings already on the machine.
func (c *Controller) prune() {
	policy := c.config.Policies.Current()
	if policy.RetentionDays <= 0 || policy.DiskQuotaMB <= 0 {
		return
	}
	deleted, err := c.config.Sessions.PruneRecordings(time.Duration(policy.RetentionDays)*24*time.Hour, int64(policy.DiskQuotaMB)<<20)
	if err != nil && len(deleted) == 0 {
		return
	}
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	for _, meta := range deleted {
		c.ledger.Deleted[meta.ID] = meta.EndReason
	}
	if len(deleted) > 0 {
		_ = c.ledger.save() // unsaved, a restart reports these as deleted for quota
	}
}
