package filetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type nativeTestLease struct {
	address    string
	expires    time.Time
	closed     atomic.Bool
	closeErr   error
	daemonDown *atomic.Bool // the lease's streams go through a local daemon that is restarting
	downErr    error
}

func (l *nativeTestLease) OpenTransferStream(ctx context.Context) (net.Conn, error) {
	if l.daemonDown != nil && l.daemonDown.Load() {
		return nil, l.downErr
	}
	if l.closed.Load() || !l.expires.After(time.Now()) {
		return nil, net.ErrClosed
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", l.address)
}
func (l *nativeTestLease) Close() error { l.closed.Store(true); return l.closeErr }

func TestNativeTransferReauthorizesLostConnectionWithoutNewBatch(t *testing.T) {
	// restarting: the renewal first finds the local daemon's socket gone, as while an update restarts
	// it (F14-02: two 50 MiB sends in flight across pb update failed instead of resuming).
	// stopping: the old daemon, shutting down for the update, answers the stream with 503 first.
	for _, mode := range []string{"allowed", "denied", "restarting", "stopping"} {
		t.Run(mode, func(t *testing.T) {
			denied := mode == "denied"
			daemonDown := 0
			var restarting atomic.Bool
			var downErr error = &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ENOENT)}
			if mode == "stopping" {
				downErr = &localapi.RemoteError{StatusCode: http.StatusServiceUnavailable, Code: "file_transfer_stream_unavailable", Message: "file transfer stream could not open"}
			}
			data := bytes.Repeat([]byte("bounded"), protocol.FileTransferChunkBytes/7+17)
			item := source(data)
			manifest := Manifest{TransferID: "ft_renew", BatchID: "batch_renew", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", Basename: item.Basename, Size: item.Size, SHA256: hex.EncodeToString(item.SHA256[:])}
			policy := Policy{Revision: "fixture", MaxFileBytes: 50 << 20, MaxBatchFiles: 10, MaxBatchBytes: 500 << 20, MaxConcurrentTransfers: 2, RetentionSeconds: 604800, DeliveryTimeoutSeconds: 600, MaxPendingSpoolBytes: 1 << 30}
			var mu sync.Mutex
			var content []byte
			var client *NativeClient
			var first *nativeTestLease
			creates, renewals := 0, 0
			oldExpired := false
			maintenanceResponses := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/authorize" {
					if r.Header.Get("Authorization") != "Bearer account" {
						w.WriteHeader(401)
						return
					}
					var binding Binding
					if json.NewDecoder(r.Body).Decode(&binding) != nil || binding != testBinding() {
						w.WriteHeader(403)
						return
					}
					renewals++
					if denied {
						w.WriteHeader(403)
						return
					}
					_ = json.NewEncoder(w).Encode(Auth{Token: "renewed", ExpiresAt: time.Now().Add(5 * time.Minute)})
					return
				}
				token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				if token != "renewed" && (token != "original" || oldExpired) {
					w.WriteHeader(401)
					return
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/file-transfers":
					creates++
					_ = json.NewEncoder(w).Encode(Batch{BatchID: manifest.BatchID, Transfers: []Manifest{manifest}})
				case r.Method == http.MethodHead:
					if oldExpired && !denied && maintenanceResponses < 6 {
						maintenanceResponses++
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
				case r.Method == http.MethodPatch:
					offset, _ := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
					chunk, err := io.ReadAll(r.Body)
					digest := sha256.Sum256(chunk)
					if err != nil || offset != int64(len(content)) || len(chunk) > protocol.FileTransferChunkBytes || r.Header.Get("Upload-Digest") != "sha256="+hex.EncodeToString(digest[:]) {
						t.Error("renewal changed durable chunk identity")
						w.WriteHeader(400)
						return
					}
					content = append(content, chunk...)
					if !oldExpired {
						oldExpired = true
						// Commit the chunk, then lose the response and original carrier.
						first.closed.Store(true)
						restarting.Store(mode == "restarting" || mode == "stopping")
						connection, _, hijackErr := w.(http.Hijacker).Hijack()
						if hijackErr != nil {
							t.Error(hijackErr)
							return
						}
						connection.Close()
						return
					}
					w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
					w.WriteHeader(204)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
					manifest.CommittedOffset = int64(len(content))
					manifest.State = "published"
					_ = json.NewEncoder(w).Encode(map[string]any{"transfer": manifest, "result": map[string]string{"code": "published", "path": "Inbox/data.bin"}})
				case r.Method == http.MethodDelete:
					w.WriteHeader(204)
				default:
					t.Error("unexpected transfer operation")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			first = &nativeTestLease{address: server.Listener.Addr().String(), expires: time.Now().Add(5 * time.Minute), daemonDown: &restarting, downErr: downErr}
			var replacement *nativeTestLease
			initial := NativeGrant{Endpoint: server.URL + "/v1/file-transfers", Auth: Auth{Token: "original", ExpiresAt: first.expires}, Binding: testBinding(), Policy: policy, Lease: first}
			var err error
			client, err = NewRenewableNativeClient(initial, func(ctx context.Context) (NativeGrant, error) {
				if (mode == "restarting" || mode == "stopping") && daemonDown < 3 {
					daemonDown++
					restarting.Store(daemonDown < 3)
					return NativeGrant{}, fmt.Errorf("renew file transfer grant: %w", downErr)
				}
				body, _ := json.Marshal(testBinding())
				request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/authorize", bytes.NewReader(body))
				request.Header.Set("Authorization", "Bearer account")
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					return NativeGrant{}, err
				}
				defer response.Body.Close()
				if response.StatusCode != 200 {
					return NativeGrant{}, &Error{StatusCode: response.StatusCode, Code: "authorization_denied"}
				}
				var auth Auth
				if err = json.NewDecoder(response.Body).Decode(&auth); err != nil {
					return NativeGrant{}, err
				}
				replacement = &nativeTestLease{address: server.Listener.Addr().String(), expires: auth.ExpiresAt}
				grant := initial
				grant.Auth = auth
				grant.Lease = replacement
				return grant, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// Keep the real HTTP maintenance failures without waiting through backoff
			// in this isolated expiry fixture; the caller deadline is tested separately.
			client.retryWait = func(ctx context.Context, _ int) error { return ctx.Err() }
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			batch, err := client.SendBatch(ctx, manifest.BatchID, "", []Source{item})
			if denied {
				var auth *nativeAuthorizationFailure
				if !errors.As(err, &auth) {
					t.Fatal("denied authorization was retried or hidden", err)
				}
			} else if err != nil || batch.BatchID != manifest.BatchID || !bytes.Equal(content, data) {
				t.Fatal("fresh grant did not complete the original batch", err)
			}
			if creates != 1 || renewals != 1 {
				t.Fatal("recovery created a new batch or repeatedly probed authorization")
			}
			if (mode == "restarting" || mode == "stopping") && daemonDown != 3 {
				t.Fatal("renewal did not wait for the restarting daemon", daemonDown)
			}
			if !denied && maintenanceResponses != 6 {
				t.Fatal("transfer did not recover beyond the former four-failure limit")
			}
			if err = client.Close(); err != nil {
				t.Fatal(err)
			}
			if !first.closed.Load() || replacement != nil && !replacement.closed.Load() {
				t.Fatal("native grant lease leaked")
			}
		})
	}
}

func TestRenewableNativeGrantRejectsChangedAuthorityAndExpiredInitialLease(t *testing.T) {
	first := &nativeTestLease{expires: time.Now().Add(-time.Second)}
	initial := NativeGrant{Endpoint: "http://fixture/v1/file-transfers", Auth: Auth{Token: "first", ExpiresAt: first.expires}, Binding: testBinding(), Lease: first}
	if _, err := NewRenewableNativeClient(initial, func(context.Context) (NativeGrant, error) {
		t.Fatal("expired initial grant granted authority")
		return NativeGrant{}, nil
	}); err == nil {
		t.Fatal("expired initial grant accepted")
	}
	first.expires = time.Now().Add(time.Second)
	initial.Auth.ExpiresAt = first.expires
	changed := &nativeTestLease{expires: time.Now().Add(5 * time.Minute)}
	client, err := NewRenewableNativeClient(initial, func(context.Context) (NativeGrant, error) {
		fresh := initial
		fresh.Auth = Auth{Token: "changed", ExpiresAt: changed.expires}
		fresh.Binding.DestinationMachineID = "foreign"
		fresh.Lease = changed
		return fresh, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first.closed.Store(true) // A lost native connection requires fresh admission.
	_, err = client.Status(t.Context(), "fixture_transfer")
	if err == nil || !changed.closed.Load() {
		t.Fatal("foreign renewed grant retained authority or lease", err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if !first.closed.Load() {
		t.Fatal("initial lease leaked after rejection")
	}
}

func TestNativeGrantRenewalRetainsInFlightResponseUntilClose(t *testing.T) {
	cleanupFailure := errors.New("fixture lease cleanup failure")
	var lost atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			if lost.CompareAndSwap(false, true) {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
				return
			}
			w.Header().Set("Upload-Offset", "0")
			return
		}
		_, _ = io.WriteString(w, "bounded response")
	}))
	defer server.Close()
	first := &nativeTestLease{address: server.Listener.Addr().String(), expires: time.Now().Add(5 * time.Minute), closeErr: cleanupFailure}
	second := &nativeTestLease{address: server.Listener.Addr().String(), expires: time.Now().Add(5 * time.Minute)}
	initial := NativeGrant{Endpoint: server.URL + "/v1/file-transfers", Auth: Auth{Token: "first", ExpiresAt: first.expires}, Binding: testBinding(), Lease: first}
	client, err := NewRenewableNativeClient(initial, func(context.Context) (NativeGrant, error) {
		next := initial
		next.Auth = Auth{Token: "second", ExpiresAt: second.expires}
		next.Lease = second
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Content(t.Context(), Manifest{TransferID: "ft_body", Size: 20, SHA256: strings.Repeat("a", 64)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Offset(t.Context(), "ft_body"); err == nil {
		t.Fatal("lost response was not reported")
	}
	if _, err = client.Offset(t.Context(), "ft_body"); err != nil {
		t.Fatal(err)
	}
	if first.closed.Load() {
		t.Fatal("renewal closed an in-flight response lease")
	}
	if _, err = io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err = response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !first.closed.Load() {
		t.Fatal("retired response lease was not released")
	}
	if err = client.Close(); !errors.Is(err, cleanupFailure) {
		t.Fatal("deferred lease cleanup failure was lost", err)
	}
	if !second.closed.Load() {
		t.Fatal("current lease survived client close")
	}
}

func TestNativeMaintenanceRecoveryKeepsDeadlineAndRejectsMixedFaults(t *testing.T) {
	state := newNativeRecovery(t.Context())
	first, cancel := state.afterFailure(state.context)
	firstDeadline, _ := first.Deadline()
	cancel()
	second, cancel := state.afterFailure(state.context)
	defer cancel()
	secondDeadline, _ := second.Deadline()
	if !firstDeadline.Equal(secondDeadline) {
		t.Fatal("repeated failure extended maintenance recovery deadline")
	}
	for _, err := range []error{errors.Join(io.EOF, syscall.EIO), errors.Join(hostdproto.ErrMaintenanceBusy, os.ErrPermission), errors.Join(io.EOF, context.Canceled)} {
		if retryableTransferFailure(err) {
			t.Fatal("mixed fault entered planned-maintenance recovery")
		}
	}
}

func TestAdmittedNativeTransferDoesNotRenewHealthyConnectionAtCredentialExpiry(t *testing.T) {
	var admitted atomic.Bool
	var renewals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer initial" && r.Header.Get("Authorization") != "Bearer early" {
			w.WriteHeader(401)
			return
		}
		admitted.Store(true)
		_ = json.NewEncoder(w).Encode(Manifest{TransferID: "transfer_live"})
	}))
	defer server.Close()
	makeGrant := func(token string) NativeGrant {
		return NativeGrant{Endpoint: server.URL + "/v1/file-transfers", Auth: Auth{Token: token, ExpiresAt: time.Now().Add(50 * time.Millisecond)}, Binding: testBinding(), Lease: &nativeTestLease{address: server.Listener.Addr().String(), expires: time.Now().Add(time.Minute)}}
	}
	client, err := NewRenewableNativeClient(makeGrant("initial"), func(context.Context) (NativeGrant, error) {
		renewals.Add(1)
		if admitted.Load() {
			return NativeGrant{}, errors.New("credential issuer temporarily unavailable")
		}
		return makeGrant("early"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Status(t.Context(), "transfer_live"); err != nil {
		t.Fatal("initial admission failed", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := client.Status(t.Context(), "transfer_live"); err != nil {
		t.Fatal("credential expiry interrupted healthy transfer", err)
	}
	if renewals.Load() != 0 {
		t.Fatal("healthy transfer renewed admission without disconnect")
	}
}
