// Package storedfiles defines encrypted, accept-first team bucket deliveries.
package storedfiles

import (
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
	"time"
)

const (
	Lifetime         = 30 * 24 * time.Hour
	MaxParts         = 65536
	MaxRecipients    = 128
	MaxMetadataBytes = 16 << 20
	MachinePath      = "/v1/machine/stored-transfers"
)

type File struct {
	Basename string `json:"basename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}
type Part struct {
	Index  int    `json:"index"`
	File   int    `json:"file"`
	Offset int64  `json:"offset"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type Recipient struct {
	MachineID  string `json:"machine_id"`
	HostPublic []byte `json:"host_public"`
}
type Metadata struct {
	Parts        []Part            `json:"parts"`
	SealedKeys   map[string][]byte `json:"sealed_keys"`
	WriterPublic []byte            `json:"writer_public"`
}
type Delivery struct {
	SourceMachineID string   `json:"source_machine_id"`
	SenderAccount   string   `json:"sender_account"`
	RequestID       string   `json:"request_id"`
	ManifestDigest  string   `json:"manifest_digest"`
	Files           []File   `json:"files"`
	Metadata        Metadata `json:"metadata"`
}
type Page struct {
	Transfers []Delivery `json:"transfers"`
}
type Range struct {
	First int `json:"first"`
	Count int `json:"count"`
}
type URLs struct {
	URLs []teambucket.Authority `json:"urls"`
}

func PartKey(prefix, id string, index int) (string, error) {
	// Reuse the bucket's validated ID and index layout; only the namespace differs.
	key, err := teambucket.RecordingSegmentKey("", id, index)
	if err != nil {
		return "", err
	}
	_ = key
	if prefix != "" {
		prefix += "/"
	}
	return fmt.Sprintf("%sv1/files/%s/%06d.part", prefix, id, index), nil
}
