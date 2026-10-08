package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

const (
	journalName       = ".paperboat-receipts.json"
	maxJournalEntries = 1024
)

var (
	errInvalidPath        = errors.New("invalid_path")
	errInvalidSize        = errors.New("invalid_size")
	errDigestMismatch     = errors.New("digest_mismatch")
	errOffsetConflict     = errors.New("offset_conflict")
	errResourceLimit      = errors.New("resource_limit")
	errStorageUnavailable = errors.New("storage_unavailable")
)

type Client interface {
	Pending(context.Context, string, int) ([]filetransfer.Manifest, error)
	Content(context.Context, filetransfer.Manifest, int64) (*http.Response, error)
	Receipt(context.Context, string, string, string) error
}

type Config struct {
	Client      Client
	MachineID   string
	SessionID   string
	Path        string
	Notify      func(string)
	PollSeconds int
}

type Inbox struct {
	config Config
	mu     sync.Mutex
}

type receipt struct {
	Digest string    `json:"digest"`
	Path   string    `json:"path"`
	At     time.Time `json:"at"`
}

type journal struct {
	Version int                `json:"version"`
	Entries map[string]receipt `json:"entries"`
}

// interruptedDownload retains the endpoint-owned partial for the next poll.
// It must never be acknowledged as a permanent delivery failure.
type interruptedDownload struct {
	cause error
	stage string
}

func (e *interruptedDownload) Error() string {
	return "file download interrupted; partial retained for resume"
}
func (e *interruptedDownload) Unwrap() error { return e.cause }
func (e *interruptedDownload) DiagnosticStage() string {
	if e.stage == "stream_open" {
		return "stream_open"
	}
	return "delivery"
}
func (*interruptedDownload) DiagnosticCode() string { return "file_transfer_failed" }

// inboxFailure keeps external transport and filesystem error strings out of
// Run's returned error while retaining the original cause for classification.
type inboxFailure struct {
	message string
	stage   string
	code    string
	storage bool
	cause   error
}

func (e *inboxFailure) Error() string           { return e.message }
func (e *inboxFailure) Unwrap() error           { return e.cause }
func (e *inboxFailure) DiagnosticStage() string { return e.stage }
func (e *inboxFailure) DiagnosticCode() string  { return e.code }
func (e *inboxFailure) Is(target error) bool    { return e.storage && target == errStorageUnavailable }

type inboxStopped struct{ cause error }

func (*inboxStopped) Error() string   { return "inbox run stopped after cancellation" }
func (e *inboxStopped) Unwrap() error { return e.cause }

type inboxFailureObservation struct {
	reference string
	faults    map[string]errorreport.Fault
	causes    map[string]error
}

func (o *inboxFailureObservation) context(ctx context.Context) context.Context {
	if o.reference == "" {
		o.reference = supportref.FromContext(ctx)
	}
	if o.reference == "" {
		o.reference = supportref.New()
	}
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, o.reference)
	}
	return ctx
}

func (o *inboxFailureObservation) observe(ctx context.Context, key, stage, code string, err error) context.Context {
	if err == nil {
		return o.context(ctx)
	}
	ctx = o.context(ctx)
	fault := errorreport.ProjectFault(ctx, "pb", "inbox", stage, code, err)
	if fault.Code == "" || fault.Outcome == "canceled" {
		return ctx
	}
	if o.faults == nil {
		o.faults = make(map[string]errorreport.Fault, 3)
		o.causes = make(map[string]error, 3)
	}
	previous := o.faults[key]
	unchanged := sameInboxFault(previous, fault)
	o.faults[key], o.causes[key] = fault, err
	if fault.SupportReference != "" && supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, fault.SupportReference)
	}
	if !unchanged && !errorreport.HTTPAttemptObserved(err) {
		errorreport.Current().ObserveFailure(ctx, "pb", "inbox", stage, code, err)
	}
	if fault.SupportReference != "" {
		o.reference = fault.SupportReference
	}
	return ctx
}

func (o *inboxFailureObservation) recovered(ctx context.Context, key string) context.Context {
	ctx = o.context(ctx)
	previous := o.faults[key]
	delete(o.faults, key)
	delete(o.causes, key)
	if previous.Code == "" {
		return ctx
	}
	ctx = supportref.WithContext(ctx, previous.SupportReference)
	errorreport.Current().Observe(ctx, "pb", "inbox", "success", -1)
	if recorder := diagnostics.FromContext(ctx); recorder != nil {
		_ = recorder.RecordWithSupportReference(previous.Stage, "recovered", "info", previous.SupportReference, map[string]string{
			"component": "paperboat-cli",
			"operation": "inbox",
		})
	}
	return ctx
}

func (o *inboxFailureObservation) handled(key string) {
	delete(o.causes, key)
}

func (o *inboxFailureObservation) finalCause() error {
	var result error
	for _, key := range []string{"pending", "delivery", "receipt"} {
		result = errors.Join(result, o.causes[key])
	}
	return result
}

func sameInboxFault(left, right errorreport.Fault) bool {
	return left.Code != "" && left.Code == right.Code && left.Stage == right.Stage &&
		left.Cause == right.Cause && left.Errno == right.Errno && left.HTTPStatus == right.HTTPStatus
}

func stoppedError(ctx context.Context, operationErr error) error {
	if ctx == nil || ctx.Err() == nil {
		return operationErr
	}
	ctxErr := ctx.Err()
	cause := context.Cause(ctx)
	if operationErr == nil && (cause == context.Canceled || cause == context.DeadlineExceeded) {
		return ctxErr
	}
	causes := []error{ctxErr}
	if cause != nil {
		causes = append(causes, cause)
	}
	if operationErr != nil {
		causes = append(causes, operationErr)
	}
	return &inboxStopped{cause: errors.Join(causes...)}
}

func controlRequestFailure(err error) error {
	return &inboxFailure{message: "inbox control request failed", stage: "control_request", code: "control_request_failed", cause: err}
}

type downloadReader struct {
	io.Reader
	err error
}

func (r *downloadReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

func closeResponseBodyOnCancel(ctx context.Context, body io.ReadCloser) func() {
	stopped := make(chan struct{})
	joined := make(chan struct{})
	var closeOnce sync.Once
	closeBody := func() { closeOnce.Do(func() { _ = body.Close() }) }
	go func() {
		defer close(joined)
		select {
		case <-ctx.Done():
			closeBody()
		case <-stopped:
		}
	}()
	return func() {
		close(stopped)
		<-joined
		closeBody()
	}
}

func New(config Config) (*Inbox, error) {
	if config.Client == nil || config.MachineID == "" || config.SessionID == "" {
		return nil, errors.New("invalid inbox configuration")
	}
	if err := EnsurePath(config.Path); err != nil {
		return nil, err
	}
	if config.PollSeconds == 0 {
		config.PollSeconds = 30
	}
	if config.PollSeconds < 1 || config.PollSeconds > 30 {
		return nil, errors.New("invalid inbox poll duration")
	}
	return &Inbox{config: config}, nil
}

func (i *Inbox) Run(ctx context.Context) error {
	var failures inboxFailureObservation
	ctx = failures.context(ctx)
	for {
		transfers, err := i.config.Client.Pending(ctx, i.config.SessionID, i.config.PollSeconds)
		if err != nil {
			failure := controlRequestFailure(err)
			ctx = failures.observe(ctx, "pending", "control_request", "control_request_failed", failure)
			if ctx.Err() != nil {
				return stoppedError(ctx, failures.finalCause())
			}
			select {
			case <-ctx.Done():
				return stoppedError(ctx, failures.finalCause())
			case <-time.After(time.Second):
			}
			continue
		}
		ctx = failures.recovered(ctx, "pending")
		for _, transfer := range transfers {
			path, deliveryErr := i.Deliver(ctx, transfer)
			if deliveryErr != nil {
				ctx = failures.observe(ctx, "delivery", "delivery", "file_transfer_failed", deliveryErr)
				if ctx.Err() != nil {
					return stoppedError(ctx, failures.finalCause())
				}
				var interrupted *interruptedDownload
				if errors.As(deliveryErr, &interrupted) {
					select {
					case <-ctx.Done():
						return stoppedError(ctx, failures.finalCause())
					case <-time.After(time.Second):
					}
					continue
				}
				code := errorCode(deliveryErr)
				if err := i.config.Client.Receipt(ctx, transfer.TransferID, code, ""); err != nil {
					failure := controlRequestFailure(err)
					ctx = failures.observe(ctx, "receipt", "control_request", "control_request_failed", failure)
					if ctx.Err() != nil {
						return stoppedError(ctx, failures.finalCause())
					}
					continue
				}
				ctx = failures.recovered(ctx, "receipt")
				failures.handled("delivery")
				continue
			}
			ctx = failures.recovered(ctx, "delivery")
			if err := i.config.Client.Receipt(ctx, transfer.TransferID, "stored", path); err != nil {
				failure := controlRequestFailure(err)
				ctx = failures.observe(ctx, "receipt", "control_request", "control_request_failed", failure)
				if ctx.Err() != nil {
					return stoppedError(ctx, failures.finalCause())
				}
				continue
			}
			ctx = failures.recovered(ctx, "receipt")
			if i.config.Notify != nil {
				i.config.Notify("Saved to " + path)
			}
		}
	}
}

func (i *Inbox) Deliver(ctx context.Context, manifest filetransfer.Manifest) (path string, resultErr error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.validateManifest(manifest); err != nil {
		return "", err
	}
	root := i.config.Path
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", storageError(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", storageError(err)
	}
	receipts, err := loadJournal(root)
	if err != nil {
		return "", storageError(err)
	}
	prior, hasPrior := receipts.Entries[manifest.TransferID]
	if hasPrior {
		if prior.Digest != manifest.SHA256 {
			return "", errDigestMismatch
		}
		name := strings.TrimPrefix(filepath.ToSlash(prior.Path), "Paperboat Inbox/")
		if name == prior.Path || filepath.Base(name) != name {
			return "", storageError(errors.New("receipt journal is corrupt"))
		}
		finalPath := filepath.Join(root, filepath.FromSlash(name))
		if info, statErr := os.Lstat(finalPath); statErr == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			if info.Size() != manifest.Size {
				return "", errDigestMismatch
			}
			matches, verifyErr := fileDigestMatches(finalPath, manifest.SHA256)
			if verifyErr != nil {
				return "", storageError(verifyErr)
			}
			if !matches {
				return "", errDigestMismatch
			}
			return prior.Path, nil
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return "", storageError(statErr)
		}
	}

	tempPath := filepath.Join(root, ".paperboat-transfer-"+manifest.TransferID+".part")
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", storageError(err)
	}
	keepTemp := true
	fileClosed := false
	defer func() {
		var cleanupErr error
		if !fileClosed {
			cleanupErr = errors.Join(cleanupErr, file.Close())
		}
		if !keepTemp {
			if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		if cleanupErr != nil {
			if resultErr != nil {
				cleanupErr = errors.Join(resultErr, cleanupErr)
			}
			resultErr = storageError(cleanupErr)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return "", storageError(err)
	}
	if info.Size() < 0 || info.Size() > manifest.Size {
		if err := file.Truncate(0); err != nil {
			return "", storageError(errors.Join(errInvalidSize, err))
		}
		return "", errInvalidSize
	}
	offset := info.Size()
	hash := sha256.New()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", storageError(err)
	}
	if copied, err := io.CopyN(hash, file, offset); err != nil || copied != offset {
		return "", storageError(errors.Join(err, io.ErrUnexpectedEOF))
	}
	if offset < manifest.Size {
		response, err := i.config.Client.Content(ctx, manifest, offset)
		if err != nil {
			var failure *filetransfer.Error
			if errors.As(err, &failure) && failure.StatusCode != http.StatusBadGateway && failure.StatusCode != http.StatusServiceUnavailable && failure.StatusCode != http.StatusGatewayTimeout {
				return "", &inboxFailure{message: "file content stream could not be opened", stage: "stream_open", code: "file_transfer_failed", cause: err}
			}
			return "", &interruptedDownload{cause: err, stage: "stream_open"}
		}
		if response == nil || response.Body == nil {
			return "", &inboxFailure{message: "file content stream could not be opened", stage: "stream_open", code: "file_transfer_failed", cause: errors.New("missing content response body")}
		}
		stopBodyClose := closeResponseBodyOnCancel(ctx, response.Body)
		defer stopBodyClose()
		if offset > 0 && response.StatusCode != http.StatusPartialContent || offset == 0 && response.StatusCode != http.StatusOK {
			return "", errOffsetConflict
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return "", storageError(err)
		}
		remaining := manifest.Size - offset
		reader := &downloadReader{Reader: response.Body}
		written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(reader, remaining+1))
		if copyErr != nil {
			if reader.err == nil || !errors.Is(copyErr, reader.err) {
				return "", storageError(copyErr)
			}
			if err := file.Sync(); err != nil {
				return "", storageError(err)
			}
			return "", &interruptedDownload{cause: copyErr}
		}
		if written < remaining {
			if err := file.Sync(); err != nil {
				return "", storageError(err)
			}
			return "", &interruptedDownload{cause: io.ErrUnexpectedEOF}
		}
		if written > remaining {
			keepTemp = false
			return "", errInvalidSize
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != manifest.SHA256 {
		keepTemp = false
		return "", errDigestMismatch
	}
	if err := file.Sync(); err != nil {
		return "", storageError(err)
	}
	if err := file.Close(); err != nil {
		fileClosed = true
		return "", storageError(err)
	}
	fileClosed = true

	var finalName, relativePath string
	if hasPrior {
		relativePath = prior.Path
		finalName = strings.TrimPrefix(filepath.ToSlash(prior.Path), "Paperboat Inbox/")
	} else {
		finalName, err = availableName(root, localBasename(manifest.Basename, runtime.GOOS))
		if err != nil {
			return "", storageError(err)
		}
		relativePath = filepath.ToSlash(filepath.Join("Paperboat Inbox", finalName))
		receipts.Entries[manifest.TransferID] = receipt{Digest: manifest.SHA256, Path: relativePath, At: time.Now().UTC()}
		boundJournal(&receipts)
		if err := saveJournal(root, receipts); err != nil {
			return "", storageError(err)
		}
	}
	if err := os.Link(tempPath, filepath.Join(root, filepath.FromSlash(finalName))); err != nil {
		return "", storageError(err)
	}
	if err := syncDir(root); err != nil {
		return "", storageError(err)
	}
	keepTemp = false
	if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", storageError(err)
	}
	if err := syncDir(root); err != nil {
		return "", storageError(err)
	}
	return relativePath, nil
}

func DefaultPath() (string, error) {
	downloads, err := DownloadsDir()
	if err == nil && filepath.IsAbs(downloads) {
		return filepath.Join(downloads, "Paperboat Inbox"), nil
	}
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		return "", errors.Join(err, homeErr)
	}
	return filepath.Join(home, "Documents", "Paperboat Inbox"), nil
}

func EnsurePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("inbox path must be an absolute clean path")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	if err := secureInboxPath(path); err != nil {
		return err
	}
	return ValidatePath(path)
}

func ValidatePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("inbox path must be an absolute clean path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("inbox path must be an existing non-symlink directory")
	}
	if err := validateInboxPath(path, info); err != nil {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=inbox reason=destination-writability-probe
	probe, err := os.CreateTemp(path, ".paperboat-inbox-probe-*")
	if err != nil {
		return errors.New("inbox path is not writable")
	}
	probePath := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		_ = os.Remove(probePath)
		return closeErr
	}
	if err := os.Remove(probePath); err != nil {
		return err
	}
	return nil
}

func fileDigestMatches(path, expected string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) == expected, nil
}

func (i *Inbox) validateManifest(manifest filetransfer.Manifest) error {
	if manifest.TransferID == "" || manifest.DestinationMachineID != i.config.MachineID || manifest.Size < 0 || manifest.Size > 50<<20 || len(manifest.SHA256) != 64 {
		return errInvalidSize
	}
	if _, err := hex.DecodeString(manifest.SHA256); err != nil || manifest.SHA256 != strings.ToLower(manifest.SHA256) {
		return errDigestMismatch
	}
	name := manifest.Basename
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") {
		return errInvalidPath
	}
	return nil
}

func localBasename(name, goos string) string {
	if goos != "windows" {
		return name
	}
	name = strings.Map(func(value rune) rune {
		if value < 32 || strings.ContainsRune(`<>:"/\|?*`, value) {
			return '_'
		}
		return value
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" {
		name = "_"
	}
	stem := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
		name = "_" + name
	}
	return name
}

func availableName(root, basename string) (string, error) {
	extension := filepath.Ext(basename)
	stem := strings.TrimSuffix(basename, extension)
	for index := 1; index <= 10000; index++ {
		name := basename
		if index > 1 {
			name = fmt.Sprintf("%s (%d)%s", stem, index, extension)
		}
		_, err := os.Lstat(filepath.Join(root, name))
		if errors.Is(err, os.ErrNotExist) {
			return name, nil
		}
		if err == nil {
			continue
		}
		return "", err
	}
	return "", errResourceLimit
}

func loadJournal(root string) (journal, error) {
	result := journal{Version: 1, Entries: make(map[string]receipt)}
	data, err := os.ReadFile(filepath.Join(root, journalName))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.Version != 1 || result.Entries == nil {
		return journal{}, errors.New("receipt journal is corrupt")
	}
	return result, nil
}

func saveJournal(root string, value journal) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(root, journalName), append(body, '\n'), atomicfile.CurrentOwnerOptions(0o600))
}

func boundJournal(value *journal) {
	for len(value.Entries) > maxJournalEntries {
		var oldestID string
		var oldest time.Time
		for id, entry := range value.Entries {
			if oldestID == "" || entry.At.Before(oldest) || entry.At.Equal(oldest) && id < oldestID {
				oldestID, oldest = id, entry.At
			}
		}
		delete(value.Entries, oldestID)
	}
}

func storageError(err error) error {
	return &inboxFailure{
		message: "inbox storage operation failed", stage: "delivery", code: "file_transfer_failed", storage: true,
		cause: err,
	}
}

func errorCode(err error) string {
	pending := []error{err}
	seen := make(map[error]struct{})
	result := ""
	leaves := 0
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return "storage_unavailable"
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return "storage_unavailable"
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return "storage_unavailable"
		}
		if value.Type().Comparable() {
			if _, ok := seen[current]; ok {
				return "storage_unavailable"
			}
			seen[current] = struct{}{}
		}

		if failure, ok := current.(*inboxFailure); ok && failure.storage {
			result, leaves = mergeReceiptCode(result, leaves, "storage_unavailable")
			continue
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 16-visited {
				return "storage_unavailable"
			}
			pending = append(pending, children...)
			continue
		case interface{ Unwrap() error }:
			if child := wrapped.Unwrap(); child != nil {
				pending = append(pending, child)
				continue
			}
		}
		code, ok := inboxReceiptCode(current)
		if !ok {
			return "storage_unavailable"
		}
		result, leaves = mergeReceiptCode(result, leaves, code)
		if result == "storage_unavailable" && code != "storage_unavailable" {
			return result
		}
	}
	if leaves == 0 || result == "" {
		return "storage_unavailable"
	}
	return result
}

func mergeReceiptCode(current string, leaves int, next string) (string, int) {
	if current != "" && current != next {
		return "storage_unavailable", leaves + 1
	}
	return next, leaves + 1
}

func inboxReceiptCode(err error) (string, bool) {
	switch failure := err.(type) {
	case *filetransfer.Error:
		if failure == nil {
			return "", false
		}
		switch failure.Code {
		case "invalid_path", "invalid_size", "digest_mismatch", "offset_conflict", "recipient_unavailable", "storage_unavailable", "resource_limit", "canceled", "delivery_timeout":
			return failure.Code, true
		}
	case *inboxFailure:
		if failure != nil && failure.storage {
			return "storage_unavailable", true
		}
	}
	if err == errInvalidPath {
		return "invalid_path", true
	}
	if err == errInvalidSize {
		return "invalid_size", true
	}
	if err == errDigestMismatch {
		return "digest_mismatch", true
	}
	if err == errOffsetConflict {
		return "offset_conflict", true
	}
	if err == errResourceLimit {
		return "resource_limit", true
	}
	if err == context.Canceled {
		return "canceled", true
	}
	if err == context.DeadlineExceeded {
		return "delivery_timeout", true
	}
	return "", false
}
