package recording

import (
	"crypto/sha256"
	"encoding/hex"
)

// Reader opens one recording's segment files in order and returns their asciicast text. It checks that
// each segment belongs to the recording, follows the previous one (index and SHA-256 chain), and that
// nothing follows the final segment, so a machine cannot drop, reorder or splice segments unseen.
type Reader struct {
	recordingID string
	generation  int64
	teamPrivate []byte
	dataKey     []byte
	next        uint32
	previous    string
	final       bool
}

// NewReader reads recordingID, sealed to generation's team recording key teamPrivate.
func NewReader(teamPrivate []byte, recordingID string, generation int64) *Reader {
	return &Reader{recordingID: recordingID, generation: generation, teamPrivate: teamPrivate}
}

// Open authenticates the next segment file and returns its asciicast lines.
func (r *Reader) Open(file []byte) ([]byte, error) {
	header, err := ParseSegmentHeader(file)
	if err != nil || r.final || header.Recording != r.recordingID || header.Index != r.next || header.KeyGeneration != r.generation || header.PreviousSHA256 != r.previous {
		return nil, ErrInvalid
	}
	if r.dataKey == nil {
		if r.dataKey, err = OpenDataKey(r.teamPrivate, r.recordingID, r.generation, header.SealedKey); err != nil {
			return nil, err
		}
	}
	header, plaintext, err := OpenSegment(file, r.dataKey)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(file)
	r.previous, r.next, r.final = hex.EncodeToString(digest[:]), r.next+1, header.Final
	return plaintext, nil
}

// Final reports whether the last segment opened was the recording's final one.
func (r *Reader) Final() bool { return r.final }

// Close clears the data key.
func (r *Reader) Close() { clear(r.dataKey) }
