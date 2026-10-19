package paste

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	transfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

// rewrite returns the paste body to emit. If the body is one-or-more local
// file paths, each is uploaded and replaced by its VM path. Any failure falls
// back to the original body (fail open) with a local notice.
func (i *Interceptor) rewrite(body []byte) ([]byte, bool) {
	lines := strings.Split(string(body), "\n")
	candidates := make([]pathCandidate, len(lines))
	owned := true
	defer func() {
		if owned {
			closeCandidates(candidates)
		}
	}()
	nonEmpty := 0
	for idx, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		nonEmpty++
		candidate, ok := parseCandidate(ln)
		if !ok {
			return body, false // not a pure file-path paste; leave untouched
		}
		resolved, file, ok := i.openLocalFile(candidate.path)
		if !ok {
			return body, false
		}
		candidate.path = resolved
		candidate.file = file
		candidates[idx] = candidate
		i.report(FileDetected)
	}
	if nonEmpty == 0 {
		return body, false
	}
	policy := i.policy.snapshot()
	maxFiles := policy.transferLimits.MaxBatchFiles
	if maxFiles > 0 && nonEmpty > maxFiles {
		i.warn("paste has %d files, over the limit of %d; sending as-is", nonEmpty, maxFiles)
		return body, false
	}
	if policy.transfer != nil {
		paths := make([]string, 0, nonEmpty)
		files := make([]*os.File, 0, nonEmpty)
		for _, candidate := range candidates {
			if candidate.file != nil {
				paths = append(paths, candidate.path)
				files = append(files, candidate.file)
			}
		}
		i.report(FileUploading)
		bodyCopy := append([]byte(nil), body...)
		linesCopy := append([]string(nil), lines...)
		candidateCopy := append([]pathCandidate(nil), candidates...)
		i.pendingUploads.Add(1)
		seq := i.uploadSeq.Add(1) - 1
		owned = false
		go i.uploadAsync(seq, policy, paths, files, bodyCopy, linesCopy, candidateCopy, nonEmpty)
		return nil, true
	}

	i.warn("file transfer unavailable; pasting original path")
	return body, false
}

func closeCandidates(candidates []pathCandidate) {
	for _, candidate := range candidates {
		if candidate.file != nil {
			_ = candidate.file.Close()
		}
	}
}

func (i *Interceptor) uploadAsync(seq uint64, policy policySnapshot, paths []string, files []*os.File, body []byte, lines []string, candidates []pathCandidate, nonEmpty int) {
	defer func() {
		_ = closeFiles(files)
	}()
	originalBody := body
	ctx := i.ctx
	if i.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, i.timeout)
		defer cancel()
	}
	uploaded := false
	sources, err := transfer.PrepareDescriptors(paths, files, policy.transferLimits)
	if err == nil {
		batchID, batchErr := transfer.NewBatchID()
		if batchErr != nil {
			err = batchErr
		} else {
			var batch transfer.Batch
			batch, err = policy.transfer.SendBatch(ctx, batchID, policy.sessionID, sources)
			uploaded = err == nil
			if err == nil && len(batch.Paths) != nonEmpty {
				err = errors.New("helper returned incomplete file batch")
			}
			if err == nil {
				out := make([]string, len(lines))
				result := 0
				for idx, line := range lines {
					if strings.TrimSpace(line) == "" {
						out[idx] = line
						continue
					}
					candidate := candidates[idx]
					var replacement string
					replacement, err = quoteRemotePath(batch.Paths[result])
					if err != nil {
						break
					}
					out[idx] = line[:candidate.start] + replacement + line[candidate.end:]
					result++
				}
				if err == nil {
					body = []byte(strings.Join(out, "\n"))
				}
			}
		}
	}
	closeErr := closeFiles(files)
	files = nil
	closeFailed := closeErr != nil && uploaded
	if closeErr != nil {
		err = errors.Join(err, closeErr)
		if uploaded {
			body = originalBody
		}
	}
	framed := make([]byte, 0, len(startMarker)+len(body)+len(endMarker))
	framed = append(framed, startMarker...)
	framed = append(framed, body...)
	framed = append(framed, endMarker...)
	// Give bytes already accepted by Write a chance to reach the worker before
	// publishing the replacement. This preserves terminal stream order when a
	// paste upload completes between two small input writes.
	if DefaultPartialFlushDelay > 0 {
		timer := time.NewTimer(DefaultPartialFlushDelay)
		select {
		case <-timer.C:
		case <-i.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
	}
	select {
	case i.completed <- uploadCompletion{seq: seq, framed: framed, err: err, uploaded: uploaded, closeFailed: closeFailed}:
	case <-i.ctx.Done():
	}
}

func (i *Interceptor) reportUploadFailure(err error, uploaded, closeFailed bool) {
	reference := ""
	if i.failureReporter != nil {
		if safeReference := i.failureReporter(err); supportref.Valid(safeReference) {
			reference = safeReference
		}
	}
	if closeFailed {
		i.warn("file uploaded, but the local file could not be closed safely; pasting original path%s", supportReferenceSuffix(reference))
	} else if uploaded {
		i.warn("file uploaded, but its path could not be inserted safely; pasting original path%s", supportReferenceSuffix(reference))
	} else {
		i.warn("file upload failed; pasting original path%s", supportReferenceSuffix(reference))
	}
}

func supportReferenceSuffix(reference string) string {
	if reference == "" {
		return ""
	}
	return "; support reference: " + reference
}

func (i *Interceptor) report(event LifecycleEvent) {
	if i.lifecycle != nil {
		i.lifecycle(event)
	}
}

// openLocalFile reports whether p points at an existing local regular file,
// honoring configured watch dirs when set.
func (i *Interceptor) openLocalFile(p string) (string, *os.File, bool) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return "", nil, false
	}
	pathInfo, err := os.Lstat(p)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return "", nil, false
	}
	file, err := os.Open(p)
	if err != nil {
		return "", nil, false
	}
	fail := func() (string, *os.File, bool) {
		_ = file.Close()
		return "", nil, false
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return fail()
	}
	resolved := p
	watchPath, watchErr := filepath.EvalSymlinks(p)
	if watchErr != nil {
		return fail()
	}
	if len(i.watchDirs) == 0 {
		if i.matchesTempPattern(resolved) {
			return resolved, file, true
		}
		return fail()
	}
	for _, d := range i.watchDirs {
		resolvedDir, dirErr := filepath.EvalSymlinks(d)
		if dirErr != nil {
			continue
		}
		resolvedDir, dirErr = filepath.Abs(resolvedDir)
		if dirErr == nil && within(resolvedDir, watchPath) {
			if i.matchesTempPattern(resolved) {
				return resolved, file, true
			}
			return fail()
		}
	}
	return fail()
}

func (i *Interceptor) matchesTempPattern(p string) bool {
	if len(i.tempPatterns) == 0 {
		return true
	}
	base := filepath.Base(p)
	normalized := filepath.ToSlash(p)
	for _, pattern := range i.tempPatterns {
		baseMatch, baseErr := filepath.Match(pattern, base)
		pathMatch, pathErr := path.Match(filepath.ToSlash(pattern), normalized)
		if (baseErr == nil && baseMatch) || (pathErr == nil && pathMatch) {
			return true
		}
	}
	return false
}

func (i *Interceptor) warn(format string, args ...any) {
	message := fmt.Sprintf("\r\n[paperboat] "+format+"\r\n", args...)
	n, err := io.WriteString(i.notify, message)
	if err == nil && n != len(message) {
		err = io.ErrShortWrite
	}
	if err != nil && i.failureReporter != nil {
		_ = i.failureReporter(err)
	}
}

func closeFiles(files []*os.File) error {
	var err error
	for _, file := range files {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	return err
}
