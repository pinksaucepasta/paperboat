//go:build darwin || linux || windows

package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/inbox"
)

const (
	// inboxObservationLimit is how many files one observation lists, newest first.
	inboxObservationLimit = 200
	// inboxResendAfter repeats an unchanged listing, so the control plane's copy never looks abandoned.
	inboxResendAfter = 5 * time.Minute
)

type inboxItem struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ReceivedAt time.Time `json:"received_at"`
}

// inboxObservation is what the machine reports about its Paperboat Inbox: names, sizes and times,
// never contents.
type inboxObservation struct {
	ObservedAt time.Time   `json:"observed_at"`
	Total      int         `json:"total"`
	Items      []inboxItem `json:"items"`
}

// scanInbox lists the delivered files of the Inbox folder as an observation (newest 200 of Total).
func scanInbox(root string, now time.Time) (*inboxObservation, error) {
	files, err := inbox.Files(root)
	if err != nil {
		return nil, err
	}
	observation := &inboxObservation{ObservedAt: now.UTC(), Total: len(files), Items: make([]inboxItem, 0, min(len(files), inboxObservationLimit))}
	for _, file := range files {
		if len(observation.Items) == inboxObservationLimit {
			break
		}
		observation.Items = append(observation.Items, inboxItem{ID: file.ID, Name: file.Name, Size: file.Size, ReceivedAt: file.ReceivedAt})
	}
	return observation, nil
}

// inboxFingerprint identifies a listing's content, ignoring when it was observed.
func inboxFingerprint(observation *inboxObservation) string {
	hash := sha256.New()
	for _, item := range observation.Items {
		hash.Write([]byte(item.ID + "\x00" + item.Name + "\x00" + item.ReceivedAt.Format(time.RFC3339Nano) + "\x00"))
		hash.Write([]byte{byte(item.Size >> 56), byte(item.Size >> 48), byte(item.Size >> 40), byte(item.Size >> 32), byte(item.Size >> 24), byte(item.Size >> 16), byte(item.Size >> 8), byte(item.Size)})
	}
	hash.Write([]byte{byte(observation.Total >> 24), byte(observation.Total >> 16), byte(observation.Total >> 8), byte(observation.Total)})
	return hex.EncodeToString(hash.Sum(nil))
}

// inboxObservationFor returns the listing to send with this heartbeat, or nil when nothing changed
// recently. The caller marks it sent only after the control plane accepted the heartbeat.
func (s *runtimeObservationSender) inboxObservationFor(now time.Time) (*inboxObservation, string) {
	if s.inboxPath == "" {
		return nil, ""
	}
	observation, err := scanInbox(s.inboxPath, now)
	if err != nil {
		// A missing or unreadable folder is an empty Inbox here; `pb doctor` reports the cause.
		return nil, ""
	}
	fingerprint := inboxFingerprint(observation)
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	if fingerprint == s.inboxSent && now.Sub(s.inboxSentAt) < inboxResendAfter {
		return nil, ""
	}
	return observation, fingerprint
}

func (s *runtimeObservationSender) inboxObservationSent(fingerprint string, at time.Time) {
	if fingerprint == "" {
		return
	}
	s.inboxMu.Lock()
	s.inboxSent, s.inboxSentAt = fingerprint, at
	s.inboxMu.Unlock()
}
