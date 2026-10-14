package installsource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// MaxInputBytes is the maximum size accepted from a manual installation input.
const MaxInputBytes int64 = (512 + 8) << 20

const (
	inputHTTPTimeout     = 15 * time.Minute
	inputProbeTimeout    = 5 * time.Second
	inputProbeWaitDelay  = 250 * time.Millisecond
	maxInputProbeOutput  = 4 << 10
	maxInputRedirects    = 5
	inputCopyBufferSize  = 32 << 10
	paperboatModulePath  = "github.com/pinksaucepasta/paperboat"
	paperboatCommandPath = "github.com/pinksaucepasta/paperboat/cmd/pb"
)

var ErrInput = errors.New("manual input is invalid or could not be staged")

type inputError struct{ cause error }

func (e inputError) Error() string { return ErrInput.Error() }

func (e inputError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrInput}
	}
	return []error{ErrInput, e.cause}
}

func inputFailure(err error) error {
	if err == nil {
		return nil
	}
	return inputError{cause: err}
}

// StagedInput identifies the private copy that install code may inspect and
// verify. Staging never executes the supplied bytes.
type StagedInput struct {
	Path   string
	SHA256 string
	Length int64
}

// StageInput copies a local file or downloads an HTTPS resource into directory.
// directory must already exist; unrelated files there are left untouched.
func StageInput(ctx context.Context, input, directory string, client *http.Client) (StagedInput, error) {
	if ctx == nil || input == "" {
		return StagedInput{}, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return StagedInput{}, inputFailure(err)
	}
	stageDir, err := existingStageDirectory(directory)
	if err != nil {
		return StagedInput{}, err
	}
	remote, parsed, err := parseInputURL(input)
	if err != nil {
		return StagedInput{}, err
	}
	if remote {
		return stageURL(ctx, parsed, stageDir, client)
	}
	return stageLocal(ctx, input, stageDir)
}

// InspectBuild accepts only a current-platform Paperboat Go CLI executable.
// It reads Go build metadata and never runs the supplied executable. version
// is supplied by the caller that owns the version authority, such as a signed
// release index.
func InspectBuild(path, version string) (Source, error) {
	if path == "" {
		return Source{}, ErrInput
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Source{}, sourceIOFailure(err)
	}
	before, err := os.Lstat(absPath)
	if err != nil {
		return Source{}, sourceIOFailure(err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return Source{}, ErrInput
	}
	if before.Size() < 1 || before.Size() > MaxBytes {
		return Source{}, ErrInput
	}
	executable, err := os.Open(absPath)
	if err != nil {
		return Source{}, sourceIOFailure(err)
	}
	defer executable.Close()
	opened, err := executable.Stat()
	if err != nil {
		return Source{}, sourceIOFailure(err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != before.Size() || opened.Size() > MaxBytes {
		return Source{}, ErrInput
	}
	info, err := buildinfo.Read(executable)
	if err != nil {
		return Source{}, sourceIOFailure(err)
	}
	if info.Main.Path != paperboatModulePath || info.Path != paperboatCommandPath {
		return Source{}, ErrInput
	}
	source, err := Inspect(absPath, version, Custom)
	if err != nil {
		return Source{}, err
	}
	current, err := os.Lstat(absPath)
	if err != nil {
		return Source{}, sourceIOFailure(err)
	}
	if !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) || current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) {
		return Source{}, ErrInput
	}
	return source, nil
}

// InspectCandidate validates a selected executable before asking that same
// user-selected program for its JSON version. It is only appropriate for an
// explicit custom-input or already-installed-binary workflow; official
// artifacts must use the version authenticated by their signed index.
func InspectCandidate(ctx context.Context, path string) (Source, error) {
	if ctx == nil {
		return Source{}, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return Source{}, inputFailure(err)
	}
	source, err := InspectBuild(path, "probe")
	if err != nil {
		return Source{}, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Source{}, inputFailure(err)
	}
	if err := source.Verify(absPath); err != nil {
		return Source{}, ErrInput
	}

	probeCtx, cancel := context.WithTimeout(ctx, inputProbeTimeout)
	defer cancel()
	command := exec.CommandContext(probeCtx, absPath, "--json", "--version")
	var output boundedProbeOutput
	output.cancel = cancel
	command.Stdin = strings.NewReader("")
	command.Stdout = &output
	command.Stderr = io.Discard
	command.WaitDelay = inputProbeWaitDelay
	if err := command.Run(); err != nil {
		if output.overflow {
			return Source{}, ErrInput
		}
		if probeCtx.Err() != nil {
			return Source{}, inputFailure(probeCtx.Err())
		}
		return Source{}, ErrInput
	}
	if err := probeCtx.Err(); err != nil {
		return Source{}, inputFailure(err)
	}
	if output.overflow {
		return Source{}, ErrInput
	}
	version, err := candidateVersion(output.data)
	if err != nil {
		return Source{}, err
	}
	source.Version = version
	if err := source.Validate(); err != nil {
		return Source{}, ErrInput
	}
	if err := source.Verify(absPath); err != nil {
		return Source{}, ErrInput
	}
	return source, nil
}

type boundedProbeOutput struct {
	data     []byte
	overflow bool
	cancel   context.CancelFunc
}

func (o *boundedProbeOutput) Write(p []byte) (int, error) {
	remaining := maxInputProbeOutput - len(o.data)
	count := min(len(p), remaining)
	if remaining > 0 {
		o.data = append(o.data, p[:count]...)
	}
	if count < len(p) {
		o.overflow = true
		if o.cancel != nil {
			o.cancel()
		}
		return count, errors.New("candidate version output exceeds limit")
	}
	return len(p), nil
}

func candidateVersion(output []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	top, err := readProbeObject(decoder, "schema_version", "ok", "data")
	if err != nil || probeJSONEOF(decoder) != nil {
		return "", ErrInput
	}
	var schema string
	var ok bool
	if json.Unmarshal(top["schema_version"], &schema) != nil || json.Unmarshal(top["ok"], &ok) != nil {
		return "", ErrInput
	}
	dataDecoder := json.NewDecoder(bytes.NewReader(top["data"]))
	data, err := readProbeObject(dataDecoder, "command", "version")
	if err != nil || probeJSONEOF(dataDecoder) != nil {
		return "", ErrInput
	}
	var command, version string
	if json.Unmarshal(data["command"], &command) != nil || json.Unmarshal(data["version"], &version) != nil || schema != "1.0" || !ok || command != "pb" {
		return "", ErrInput
	}
	return version, nil
}

func readProbeObject(decoder *json.Decoder, expectedKeys ...string) (map[string]json.RawMessage, error) {
	allowed := make(map[string]struct{}, len(expectedKeys))
	for _, key := range expectedKeys {
		allowed[key] = struct{}{}
	}
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrInput
	}
	fields := make(map[string]json.RawMessage, len(expectedKeys))
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, ErrInput
		}
		if _, ok := allowed[key]; !ok {
			return nil, ErrInput
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, ErrInput
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, ErrInput
		}
		fields[key] = value
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || len(fields) != len(expectedKeys) {
		return nil, ErrInput
	}
	return fields, nil
}

func probeJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrInput
	}
	return nil
}

func existingStageDirectory(directory string) (string, error) {
	if directory == "" {
		return "", ErrInput
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", inputFailure(err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", inputFailure(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrInput
	}
	return abs, nil
}

func parseInputURL(input string) (bool, *url.URL, error) {
	parsed, err := url.Parse(input)
	if err != nil {
		if strings.Contains(input, "://") || strings.HasPrefix(strings.ToLower(input), "https:") {
			return false, nil, ErrInput
		}
		return false, nil, nil
	}
	if parsed.Scheme == "" {
		return false, nil, nil
	}
	if runtime.GOOS == "windows" && filepath.VolumeName(input) != "" {
		return false, nil, nil
	}
	if !strings.EqualFold(parsed.Scheme, "https") || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || strings.Contains(input, "#") {
		return false, nil, ErrInput
	}
	parsed.Scheme = "https"
	return true, parsed, nil
}

func stageLocal(ctx context.Context, input, directory string) (StagedInput, error) {
	abs, err := filepath.Abs(input)
	if err != nil {
		return StagedInput{}, inputFailure(err)
	}
	before, err := os.Lstat(abs)
	if err != nil {
		return StagedInput{}, inputFailure(err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 1 || before.Size() > MaxInputBytes {
		return StagedInput{}, ErrInput
	}
	source, err := os.Open(abs)
	if err != nil {
		return StagedInput{}, inputFailure(err)
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil {
		return StagedInput{}, inputFailure(err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() < 1 || opened.Size() > MaxInputBytes {
		return StagedInput{}, ErrInput
	}
	stage, err := stageReader(ctx, directory, suffixForLocal(input), source, -1)
	if err != nil {
		return StagedInput{}, err
	}
	after, err := source.Stat()
	if err != nil {
		_ = os.Remove(stage.Path)
		return StagedInput{}, inputFailure(err)
	}
	if after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || stage.Length != opened.Size() {
		_ = os.Remove(stage.Path)
		return StagedInput{}, ErrInput
	}
	return stage, nil
}

func stageURL(ctx context.Context, parsed *url.URL, directory string, client *http.Client) (StagedInput, error) {
	if parsed == nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return StagedInput{}, ErrInput
	}
	requestCtx, cancel := context.WithTimeout(ctx, inputHTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return StagedInput{}, ErrInput
	}
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Del("Authorization")
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Cookie")

	boundedClient := boundedInputClient(client)
	response, err := boundedClient.Do(request)
	if err != nil {
		return StagedInput{}, inputFailure(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Request == nil || !safeRedirectURL(response.Request.URL) {
		return StagedInput{}, ErrInput
	}
	if response.ContentLength > MaxInputBytes {
		return StagedInput{}, ErrInput
	}
	return stageReader(requestCtx, directory, suffixForURL(parsed), response.Body, response.ContentLength)
}

func boundedInputClient(client *http.Client) *http.Client {
	bounded := &http.Client{}
	if client != nil {
		*bounded = *client
	}
	if bounded.Timeout <= 0 || bounded.Timeout > inputHTTPTimeout {
		bounded.Timeout = inputHTTPTimeout
	}
	originalCheck := bounded.CheckRedirect
	bounded.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > maxInputRedirects || !safeRedirectURL(request.URL) {
			return ErrInput
		}
		request.Header.Del("Authorization")
		request.Header.Del("Proxy-Authorization")
		request.Header.Del("Cookie")
		request.Header.Del("Referer")
		if originalCheck != nil {
			if err := originalCheck(request, via); err != nil {
				return err
			}
		}
		if !safeRedirectURL(request.URL) {
			return ErrInput
		}
		request.Header.Del("Authorization")
		request.Header.Del("Proxy-Authorization")
		request.Header.Del("Cookie")
		request.Header.Del("Referer")
		return nil
	}
	// Staging must not use ambient cookies from a caller's jar.
	bounded.Jar = nil
	return bounded
}

func safeRedirectURL(parsed *url.URL) bool {
	return parsed != nil && strings.EqualFold(parsed.Scheme, "https") && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func suffixForLocal(input string) string {
	return allowedSuffix(filepath.Ext(filepath.Base(input)))
}

func suffixForURL(parsed *url.URL) string {
	if parsed == nil {
		return ""
	}
	return allowedSuffix(path.Ext(path.Base(parsed.Path)))
}

func allowedSuffix(suffix string) string {
	switch strings.ToLower(suffix) {
	case ".pkg", ".pb-update", ".exe":
		return strings.ToLower(suffix)
	default:
		return ""
	}
}

func stageReader(ctx context.Context, directory, suffix string, source io.Reader, contentLength int64) (staged StagedInput, retErr error) {
	temporary, err := os.CreateTemp(directory, "input-*"+suffix)
	if err != nil {
		return StagedInput{}, inputFailure(err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if retErr == nil {
			return
		}
		var cleanupErrors []error
		if !closed {
			if closeErr := temporary.Close(); closeErr != nil {
				cleanupErrors = append(cleanupErrors, closeErr)
			}
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, removeErr)
		}
		if len(cleanupErrors) > 0 {
			retErr = inputFailure(errors.Join(retErr, errors.Join(cleanupErrors...)))
		}
	}()

	length, digest, err := copyInput(ctx, temporary, source)
	if err != nil {
		return StagedInput{}, err
	}
	if contentLength >= 0 && length != contentLength {
		if length < contentLength {
			return StagedInput{}, inputFailure(io.ErrUnexpectedEOF)
		}
		return StagedInput{}, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return StagedInput{}, inputFailure(err)
	}
	if err := temporary.Sync(); err != nil {
		return StagedInput{}, inputFailure(err)
	}
	if err := temporary.Close(); err != nil {
		closed = true
		return StagedInput{}, inputFailure(err)
	}
	closed = true
	return StagedInput{Path: temporaryPath, SHA256: digest, Length: length}, nil
}

func copyInput(ctx context.Context, destination io.Writer, source io.Reader) (int64, string, error) {
	hash := sha256.New()
	writer := io.MultiWriter(destination, hash)
	buffer := make([]byte, inputCopyBufferSize)
	var length int64
	zeroReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", inputFailure(err)
		}
		allowed := MaxInputBytes - length + 1
		readBuffer := buffer
		if allowed < int64(len(readBuffer)) {
			readBuffer = readBuffer[:allowed]
		}
		count, readErr := source.Read(readBuffer)
		if count > 0 {
			if int64(count) > MaxInputBytes-length {
				return 0, "", ErrInput
			}
			if err := ctx.Err(); err != nil {
				return 0, "", inputFailure(err)
			}
			written, writeErr := writer.Write(readBuffer[:count])
			if writeErr != nil {
				return 0, "", inputFailure(writeErr)
			}
			if written != count {
				return 0, "", inputFailure(io.ErrShortWrite)
			}
			length += int64(count)
			zeroReads = 0
		} else if readErr == nil {
			zeroReads++
			if zeroReads >= 100 {
				return 0, "", inputFailure(io.ErrNoProgress)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return 0, "", inputFailure(readErr)
		}
	}
	if length == 0 {
		return 0, "", ErrInput
	}
	return length, hex.EncodeToString(hash.Sum(nil)), nil
}
