// Package teambucket is the wire contract of a team's own S3-compatible bucket. The team configures it once
// (endpoint, bucket, region, credentials stored encrypted by the server); machines archive their terminal recordings in
// it. Only ciphertext ever goes in the bucket; machines and browsers reach it through short-lived presigned requests the
// server signs, and never hold the credentials.
package teambucket

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Object layout under the team's prefix.
const (
	recordingsDir = "v1/recordings"
	verifyDir     = "v1/verify"
)

// Bounds shared by producer and consumer.
const (
	MaxSegmentsPerBatch = 16
	// MaxObjectBytes bounds one presigned upload (a recording segment file; see cli/internal/recording).
	MaxObjectBytes = 8 << 20
	// URLLifetime is how long a presigned request stays valid.
	URLLifetime = 10 * time.Minute
)

// Config is the team's bucket as the console and CLI read it. The secret key is never returned.
type Config struct {
	Endpoint    string    `json:"endpoint"`
	Region      string    `json:"region"`
	Bucket      string    `json:"bucket"`
	Prefix      string    `json:"prefix"`
	PathStyle   bool      `json:"path_style"`
	AccessKeyID string    `json:"access_key_id"`
	CheckedAt   time.Time `json:"checked_at"`
}

// Input saves the bucket (PUT /v1/teams/{team}/bucket). An empty SecretAccessKey keeps the stored one when the
// bucket already exists. Saving a changed bucket checks it with a probe object; a failed check is a refusal that names
// the step, and nothing is stored.
type Input struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix"`
	PathStyle       bool   `json:"path_style"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
}

// Authority is one presigned request: the URL and the headers the caller must send with it unchanged.
type Authority struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Segment describes one recording segment file to upload.
type Segment struct {
	Index  int    `json:"index"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"` // hex of the file's bytes
}

// UploadRequest is POST /v1/machine/recordings/{id}/upload-urls.
type UploadRequest struct {
	Segments []Segment `json:"segments"`
}

// UploadURLs answers an UploadRequest: one upload per segment, in order.
type UploadURLs struct {
	Uploads []Authority `json:"uploads"`
}

// ArchivedRequest is POST /v1/machine/recordings/{id}/archived: segments 0..Segments-1 are in the bucket.
type ArchivedRequest struct {
	Segments int `json:"segments"`
}

// Paths of the machine-credential endpoints.
func UploadURLsPath(recordingID string) string {
	return "/v1/machine/recordings/" + recordingID + "/upload-urls"
}
func ArchivedPath(recordingID string) string {
	return "/v1/machine/recordings/" + recordingID + "/archived"
}

var (
	ErrInvalid = errors.New("invalid bucket configuration")

	regionPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,32}$`)
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixPattern = regexp.MustCompile(`^([A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*)?$`)
	keyPattern    = regexp.MustCompile(`^[A-Za-z0-9+/=_.-]{1,128}$`)
	idPattern     = regexp.MustCompile(`^[a-z]{2,8}_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hexSHA256     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Validate checks the fields the server stores. The endpoint must be an https URL without credentials, path or query.
func (in Input) Validate() error {
	u, err := url.Parse(in.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || len(in.Endpoint) > 512 {
		return fmt.Errorf("%w: the endpoint must be an https address without a path", ErrInvalid)
	}
	if !publicHost(u.Hostname()) {
		return fmt.Errorf("%w: the endpoint must be a public host name or address", ErrInvalid)
	}
	switch {
	case !regionPattern.MatchString(in.Region):
		return fmt.Errorf("%w: the region is letters, digits and dashes", ErrInvalid)
	case !bucketPattern.MatchString(in.Bucket):
		return fmt.Errorf("%w: the bucket name is not valid", ErrInvalid)
	case len(in.Prefix) > 128 || !prefixPattern.MatchString(in.Prefix) || slices.Contains(strings.Split(in.Prefix, "/"), "..") || slices.Contains(strings.Split(in.Prefix, "/"), "."):
		return fmt.Errorf("%w: the prefix is folder names separated by slashes", ErrInvalid)
	case !keyPattern.MatchString(in.AccessKeyID):
		return fmt.Errorf("%w: the access key ID is not valid", ErrInvalid)
	case len(in.SecretAccessKey) > 256:
		return fmt.Errorf("%w: the secret access key is too long", ErrInvalid)
	}
	return nil
}

// publicHost: a dotted host name, or an IP literal that is not private, loopback, link-local or unspecified. Machines
// and browsers fetch presigned URLs from this host themselves, so it must not point into anyone's network by name.
func publicHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
	}
	return strings.Contains(host, ".") && host != "localhost" && !strings.HasSuffix(host, ".localhost") && !strings.HasSuffix(host, ".internal") && !strings.HasSuffix(host, ".local")
}

func join(prefix, rest string) string {
	if prefix == "" {
		return rest
	}
	return strings.Trim(prefix, "/") + "/" + rest
}

// RecordingSegmentKey is the object holding one recording segment file (the machine's NNNNNN.seg, unchanged).
func RecordingSegmentKey(prefix, recordingID string, index int) (string, error) {
	if !idPattern.MatchString(recordingID) || index < 0 || index > 999999 {
		return "", ErrInvalid
	}
	return join(prefix, fmt.Sprintf("%s/%s/%06d.seg", recordingsDir, recordingID, index)), nil
}

// RecordingPrefixKey is the folder of one recording's segments.
func RecordingPrefixKey(prefix, recordingID string) (string, error) {
	if !idPattern.MatchString(recordingID) {
		return "", ErrInvalid
	}
	return join(prefix, recordingsDir+"/"+recordingID+"/"), nil
}

// VerifyKey is the probe object a save writes, reads and deletes.
func VerifyKey(prefix, nonce string) string { return join(prefix, verifyDir+"/"+nonce) }

// Validate checks one upload description.
func (s Segment) Validate() error {
	if s.Index < 0 || s.Index > 999999 || s.Bytes < 1 || s.Bytes > MaxObjectBytes || !hexSHA256.MatchString(s.SHA256) {
		return ErrInvalid
	}
	return nil
}
