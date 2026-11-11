package teambucket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	shared "github.com/pinksaucepasta/paperboat/shared/teambucket"
)

// fakeS3 is a path-style S3 that keeps objects in memory and can refuse one operation.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	refuse  string // method that answers 403 AccessDenied
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == f.refuse {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>not allowed</Message></Error>`)
		return
	}
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[r.URL.Path] = body
	case http.MethodGet:
		body, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
			return
		}
		_, _ = w.Write(body)
	case http.MethodDelete:
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
}

func newTestStore(t *testing.T, fake *fakeS3) (*Store, *http.Client) {
	t.Helper()
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)
	cfg := shared.Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "team-bucket", Prefix: "paperboat", PathStyle: true, AccessKeyID: "AKIAEXAMPLE", UseRecordings: true}
	store, err := newStore(context.Background(), cfg, "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return store, server.Client()
}

func TestProbeNamesTheFailingStep(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	store, _ := newTestStore(t, fake)
	if err := store.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.objects) != 0 {
		t.Fatalf("the probe left %d objects behind", len(fake.objects))
	}
	for method, step := range map[string]string{http.MethodPut: "write", http.MethodGet: "read", http.MethodDelete: "delete"} {
		fake.refuse = method
		var probe *ProbeError
		err := store.Probe(context.Background())
		if !errors.As(err, &probe) || probe.Step != step || !strings.Contains(probe.Message, "AccessDenied") {
			t.Errorf("%s refused: %v", method, err)
		}
		fake.refuse = ""
	}
}

// A presigned upload is bound to the size and SHA-256 the caller named, and works with exactly the headers returned.
func TestPresignedUploadCarriesItsBindings(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	store, client := newTestStore(t, fake)
	body := []byte("ciphertext segment")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	key, _ := shared.RecordingSegmentKey("paperboat", "rec_00000000-0000-4000-8000-000000000001", 3)
	auth, err := store.PresignPut(context.Background(), key, int64(len(body)), digest, shared.URLLifetime)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(auth.URL)
	if !strings.HasSuffix(u.Path, "/team-bucket/"+key) || u.Query().Get("X-Amz-Signature") == "" || u.Query().Get("X-Amz-Expires") != "600" {
		t.Fatalf("url %s", auth.URL)
	}
	if !strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), "x-amz-checksum-sha256") && auth.Headers["X-Amz-Checksum-Sha256"] == "" && auth.Headers["x-amz-checksum-sha256"] == "" {
		t.Fatalf("the digest is not bound: %+v %s", auth.Headers, u.RawQuery)
	}
	request, _ := http.NewRequest(http.MethodPut, auth.URL, strings.NewReader(string(body)))
	for name, value := range auth.Headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("upload %v %v", response, err)
	}
	if string(fake.objects["/team-bucket/"+key]) != string(body) {
		t.Fatal("the object was not stored under the key")
	}
	get, err := store.PresignGet(context.Background(), key, shared.URLLifetime)
	if err != nil || !strings.Contains(get.URL, "X-Amz-Signature=") {
		t.Fatalf("get %+v %v", get, err)
	}
	if _, err := store.PresignPut(context.Background(), key, 0, digest, shared.URLLifetime); !errors.Is(err, shared.ErrInvalid) {
		t.Fatalf("zero size = %v", err)
	}
	if _, err := store.PresignPut(context.Background(), key, 10, "nothex", shared.URLLifetime); !errors.Is(err, shared.ErrInvalid) {
		t.Fatalf("bad digest = %v", err)
	}
	if err := store.Delete(context.Background(), key); err != nil || len(fake.objects) != 0 {
		t.Fatalf("delete %v, %d left", err, len(fake.objects))
	}
	if err := store.Delete(context.Background(), key); err != nil {
		t.Fatalf("deleting a missing object is not an error: %v", err)
	}
}
