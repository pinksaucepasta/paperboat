package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type mcpSendRoot struct {
	path string
	root *os.Root
}

func (b *mcpBackend) openRoots(paths []string) error {
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			b.closeRoots()
			return errors.New("resolve --allow-send-dir: provide an accessible local directory")
		}
		root, err := os.OpenRoot(absolute)
		if err != nil {
			b.closeRoots()
			return errors.New("open --allow-send-dir: provide an accessible local directory")
		}
		b.roots = append(b.roots, mcpSendRoot{path: absolute, root: root})
	}
	return nil
}

func (b *mcpBackend) closeRoots() {
	for _, root := range b.roots {
		_ = root.root.Close()
	}
	b.roots = nil
}

// Open through os.Root, then snapshot into a private directory. Passing the
// original path to send would reopen it after authorization and permit a
// symlink swap to escape the caller's allowed directory.
func (b *mcpBackend) prepareSend(ctx context.Context, path string) (string, func(), error) {
	if !filepath.IsAbs(path) {
		return "", nil, errors.New("send_file requires an absolute local path")
	}
	for _, root := range b.roots {
		relative, err := filepath.Rel(root.path, path)
		if err != nil || !filepath.IsLocal(relative) {
			continue
		}
		source, err := openMCPSendSource(root.root, relative)
		if err != nil {
			return "", nil, errors.New("open send_file: file must exist within an allowed directory; external symlinks are refused")
		}
		defer source.Close()
		info, err := source.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > automaticTransferLimits.MaxFileBytes {
			return "", nil, errors.New("send_file requires one regular file within the Automatic transfer size limit")
		}
		return snapshotMCPSend(ctx, source, filepath.Base(path), info.Size())
	}
	return "", nil, errors.New("file is outside the allowed send directories; configure pb mcp --allow-send-dir")
}

func snapshotMCPSend(ctx context.Context, source *os.File, name string, size int64) (string, func(), error) {
	directory, err := os.MkdirTemp("", "paperboat-mcp-send-")
	if err != nil {
		return "", nil, errors.New("prepare send_file: cannot create a private temporary directory")
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	path := filepath.Join(directory, name)
	destination, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		cleanup()
		return "", nil, errors.New("prepare send_file: cannot create its temporary snapshot")
	}
	n, copyErr := io.Copy(destination, io.LimitReader(mcpCopyReader{ctx, source}, size+1))
	closeErr := destination.Close()
	if copyErr != nil || closeErr != nil || n != size {
		cleanup()
		return "", nil, errors.New("prepare send_file: copy failed, timed out or the source size changed; nothing was sent")
	}
	return path, cleanup, nil
}

type mcpCopyReader struct {
	ctx context.Context
	io.Reader
}

func (r mcpCopyReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}
