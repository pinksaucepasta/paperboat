package filetransfer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

// The wire must carry Content-Length == segment length (not chunked) for files and folders.
func TestNativePatchSendsContentLength(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func(t *testing.T) Source
		want int64
	}{
		{"file", func(t *testing.T) Source {
			t.Helper()
			return source(make([]byte, 1<<20))
		}, 1 << 20},
		{"folder", func(t *testing.T) Source {
			t.Helper()
			batch, err := Prepare([]string{makeTree(t)}, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = batch.Close() })
			return batch.Sources[0]
		}, 0}, // want filled below: min(folder.Size, nativeSegmentBytes)
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := tc.mk(t)
			want := tc.want
			if tc.name == "folder" {
				want = min(item.Size, nativeSegmentBytes)
			}
			var gotLength, gotBody atomic.Int64
			gotLength.Store(-2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotLength.Store(r.ContentLength)
				// Drain to mirror a real receiver; the assertion is on the declared length.
				n := int64(0)
				buf := make([]byte, 32<<10)
				for {
					m, err := r.Body.Read(buf)
					n += int64(m)
					if err != nil {
						break
					}
				}
				gotBody.Store(n)
				w.Header().Set("Upload-Offset", strconv.FormatInt(n, 10))
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			client := NewClient(server.URL+"/v1/file-transfers", Auth{Token: "token"}, testBinding(), server.Client())
			if _, err := client.patchNative(context.Background(), "ft_1", 0, item, func(int64) {}); err != nil {
				t.Fatal(err)
			}
			if gotLength.Load() != want {
				t.Fatalf("ContentLength = %d, want %d", gotLength.Load(), want)
			}
			if gotBody.Load() != want {
				t.Fatalf("body bytes = %d, want %d", gotBody.Load(), want)
			}
		})
	}
}

// A file longer than one segment sends the segment's length, not the file's, and the rest in the next request.
func TestNativePatchContentLengthIsTheSegmentWhenTheFileIsLonger(t *testing.T) {
	shortSegments(t)
	item := source(make([]byte, 3<<20))
	var gotLength atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLength.Store(r.ContentLength)
		n, _ := io.Copy(io.Discard, r.Body)
		w.Header().Set("Upload-Offset", strconv.FormatInt(1<<20+n, 10))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := NewClient(server.URL+"/v1/file-transfers", Auth{Token: "token"}, testBinding(), server.Client())
	if _, err := client.patchNative(context.Background(), "ft_1", 1<<20, item, func(int64) {}); err != nil {
		t.Fatal(err)
	}
	if gotLength.Load() != nativeSegmentBytes {
		t.Fatalf("ContentLength = %d, want the %d byte segment", gotLength.Load(), nativeSegmentBytes)
	}
}
