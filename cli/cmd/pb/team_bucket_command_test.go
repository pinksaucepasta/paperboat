package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/recording"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
)

func bucketCommandWith(t *testing.T, stdin string, args ...string) (teambucket.Input, error) {
	t.Helper()
	command := teamBucketCommand()
	set, _, err := command.Find([]string{"set"})
	if err != nil {
		t.Fatal(err)
	}
	set.SetIn(strings.NewReader(stdin))
	if err := set.Flags().Parse(args); err != nil {
		t.Fatal(err)
	}
	return bucketInput(set)
}

func TestBucketInputTakesTheSecretFromStandardInputAndChecksTheRest(t *testing.T) {
	base := []string{"--endpoint", "https://s3.example.test", "--region", "us-east-1", "--bucket", "team-bucket", "--access-key-id", "AKIAEXAMPLE", "--use", "recordings,files", "--secret-access-key-stdin"}
	in, err := bucketCommandWith(t, "top-secret\nignored\n", base...)
	if err != nil || in.SecretAccessKey != "top-secret" || !in.UseRecordings || !in.UseFiles || in.Prefix != "" {
		t.Fatalf("%+v %v", in, err)
	}
	if in, err = bucketCommandWith(t, "", base[:len(base)-1]...); err != nil || in.SecretAccessKey != "" {
		t.Fatalf("a change of uses keeps the stored key: %+v %v", in, err)
	}
	if _, err := bucketCommandWith(t, "", "--endpoint", "http://s3.example.test", "--region", "r", "--bucket", "team-bucket", "--access-key-id", "A", "--use", "files"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("a plain http endpoint = %v", err)
	}
	if _, err := bucketCommandWith(t, "", "--endpoint", "https://s3.example.test", "--use", "everything"); err == nil || !strings.Contains(err.Error(), "--use") {
		t.Fatalf("an unknown use = %v", err)
	}
}

func TestPrintBucketNeverCarriesASecret(t *testing.T) {
	var out bytes.Buffer
	if err := printBucket(&out, teambucket.Config{Endpoint: "https://s3.example.test", Region: "r", Bucket: "team-bucket", AccessKeyID: "AKIAEXAMPLE", UseRecordings: true}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Used for: recordings") || !strings.Contains(out.String(), "(bucket root)") || strings.Contains(strings.ToLower(out.String()), "secret") {
		t.Fatalf("%q", out.String())
	}
}

func TestDownloadSegmentChecksStatusAndSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("segment bytes"))
		case "/big":
			_, _ = w.Write(make([]byte, recording.MaxSegmentFile+1))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	get := func(path string) ([]byte, error) {
		return downloadSegment(context.Background(), server.Client(), teambucket.Authority{URL: server.URL + path})
	}
	if data, err := get("/ok"); err != nil || string(data) != "segment bytes" {
		t.Fatalf("%q %v", data, err)
	}
	if _, err := get("/denied"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("denied = %v", err)
	}
	if _, err := get("/big"); err == nil {
		t.Fatal("an oversized segment was accepted")
	}
}
