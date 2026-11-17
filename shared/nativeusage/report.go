// Package nativeusage is the machine's native-access usage report: absolute application-byte counters per
// authorized stream, UTC minute and observed path, posted to the control plane under the machine proof (the
// proof covers the body bytes, so field order is part of the contract). The server owns attribution and limits.
package nativeusage

import "time"

// Report is an absolute counter for one authorized stream, UTC minute and observed path.
type Report struct {
	AccessSessionID string    `json:"access_session_id"`
	StreamID        string    `json:"stream_id"`
	Consumer        string    `json:"consumer"`
	Mode            string    `json:"mode"`
	NodeID          string    `json:"node_id"`
	IntervalStart   time.Time `json:"interval_start"`
	IntervalEnd     time.Time `json:"interval_end"`
	UploadBytes     int64     `json:"upload_bytes"`
	DownloadBytes   int64     `json:"download_bytes"`
	// PeerAccountID names the peer of a stream the host admitted from its own signed rules (every managed SSH
	// stream, D1; exec or terminal offline). It has no access session, so AccessSessionID is the host itself.
	PeerAccountID string `json:"peer_account_id,omitempty"`
}

// Batch is one delivery: the reports plus the bytes the host could not attribute to a report.
type Batch struct {
	Reports             []Report `json:"reports"`
	UnrecordedBytes     int64    `json:"unrecorded_bytes"`
	ExpiredBytes        int64    `json:"expired_bytes"`
	RecordingIncomplete bool     `json:"recording_incomplete"`
}
