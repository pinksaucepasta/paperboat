package teambucket

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	shared "github.com/pinksaucepasta/paperboat/shared/teambucket"
)

// ObjectStore is what the server does with a team's bucket (the recordings package tests it with a fake).
type ObjectStore interface {
	Config() shared.Config
	PresignPut(ctx context.Context, key string, size int64, digestHex string, ttl time.Duration) (shared.Authority, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (shared.Authority, error)
	Delete(ctx context.Context, key string) error
}

// Store reaches one team's bucket. Presigning is offline arithmetic: it never contacts the bucket, so a bucket on a
// network only the machines reach still gets URLs. Probe, Delete and Get reads do contact it, from the server.
type Store struct {
	cfg       shared.Config
	client    *s3.Client
	presigner *s3.PresignClient
}

func newStore(ctx context.Context, cfg shared.Config, secret string, client *http.Client) (*Store, error) {
	loaded, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, secret, "")),
		awsconfig.WithHTTPClient(client),
		awsconfig.WithBaseEndpoint(cfg.Endpoint),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("configure team bucket client: %w", err)
	}
	c := s3.NewFromConfig(loaded, func(o *s3.Options) { o.UsePathStyle = cfg.PathStyle })
	return &Store{cfg: cfg, client: c, presigner: s3.NewPresignClient(c)}, nil
}

// Config is what the team saved.
func (s *Store) Config() shared.Config { return s.cfg }

func authority(signed http.Header, rawURL string) (shared.Authority, error) {
	headers := make(map[string]string, len(signed))
	for name, values := range signed {
		if strings.EqualFold(name, "host") {
			continue
		}
		if len(values) != 1 {
			return shared.Authority{}, shared.ErrInvalid
		}
		headers[name] = values[0]
	}
	return shared.Authority{URL: rawURL, Headers: headers}, nil
}

// PresignPut authorizes one upload of exactly size bytes whose SHA-256 is digest (hex), valid for ttl.
func (s *Store) PresignPut(ctx context.Context, key string, size int64, digestHex string, ttl time.Duration) (shared.Authority, error) {
	digest, err := hex.DecodeString(digestHex)
	if err != nil || len(digest) != 32 || size < 1 || size > shared.MaxObjectBytes || key == "" {
		return shared.Authority{}, shared.ErrInvalid
	}
	checksum := base64.StdEncoding.EncodeToString(digest)
	p, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key), ContentLength: aws.Int64(size), ChecksumSHA256: &checksum,
	}, func(o *s3.PresignOptions) { o.Expires = ttl })
	if err != nil {
		return shared.Authority{}, fmt.Errorf("presign team bucket upload: %w", err)
	}
	return authority(p.SignedHeader, p.URL)
}

// PresignGet authorizes one download of key, valid for ttl.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (shared.Authority, error) {
	if key == "" {
		return shared.Authority{}, shared.ErrInvalid
	}
	p, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key)},
		func(o *s3.PresignOptions) { o.Expires = ttl })
	if err != nil {
		return shared.Authority{}, fmt.Errorf("presign team bucket download: %w", err)
	}
	return authority(p.SignedHeader, p.URL)
}

// Delete removes one object; a missing object is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key)})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound") {
			return nil
		}
		return err
	}
	return nil
}

// ProbeError names the step of the bucket check that failed and what the bucket answered.
type ProbeError struct {
	Step    string // write, read or delete
	Message string // the provider's own words, shortened
}

func (e *ProbeError) Error() string {
	return "the bucket check failed at its " + e.Step + " step: " + e.Message
}

func providerMessage(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode() + ": " + shorten(api.ErrorMessage())
	}
	return shorten(err.Error())
}

func shorten(s string) string {
	if len(s) > 160 {
		return s[:160]
	}
	return s
}

// Probe writes, reads back and deletes one small object, proving the credentials may do all three.
func (s *Store) Probe(ctx context.Context) error {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	key := shared.VerifyKey(s.cfg.Prefix, hex.EncodeToString(nonce))
	body := []byte("paperboat bucket check")
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))}); err != nil {
		return &ProbeError{Step: "write", Message: providerMessage(err)}
	}
	got, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key)})
	if err == nil {
		read, readErr := io.ReadAll(io.LimitReader(got.Body, 1024))
		_ = got.Body.Close()
		if readErr != nil || !bytes.Equal(read, body) {
			err = errors.New("the object read back differs from the one written")
		}
	}
	if err != nil {
		_ = s.Delete(ctx, key)
		return &ProbeError{Step: "read", Message: providerMessage(err)}
	}
	if err := s.Delete(ctx, key); err != nil {
		return &ProbeError{Step: "delete", Message: providerMessage(err)}
	}
	return nil
}
