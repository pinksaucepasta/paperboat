package configsync

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Config repositories contain small files; bound transport work independently
// of history size. Bytes match the maximum policy batch (500 MiB); 100,000
// objects allow generous tree/history overhead for config workloads.
const localMaxObjects = 100_000
const localMaxBytes int64 = 500 << 20

func openLocalRepository(path string) (*git.Repository, error) {
	return openLocalRepositoryContext(context.Background(), path)
}
func openLocalRepositoryContext(ctx context.Context, path string) (*git.Repository, error) {
	native, err := nativeRepositoryPath(path)
	if err != nil {
		return nil, err
	}
	path = native
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, ErrRepositoryUnavailable
	}
	r, err := git.PlainOpen(path)
	if err != nil {
		return nil, ErrRepositoryUnavailable
	}
	cfg, err := r.Config()
	if err != nil || !cfg.Core.IsBare {
		return nil, ErrGitRepositoryInvalid
	}
	for _, name := range []string{"config", "HEAD", "objects", "refs"} {
		info, err := os.Lstat(filepath.Join(path, name))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrGitRepositoryInvalid
		}
	}
	// A repository's object/ref directories must not redirect outside the exact
	// granted local repository. Mounted shares are accepted at their mount path.
	if data, err := os.ReadFile(filepath.Join(path, "objects", "info", "alternates")); err == nil && len(data) > 0 {
		return nil, ErrGitRepositoryInvalid
	}
	count := 0
	for _, name := range []string{"objects", "refs"} {
		err := filepath.WalkDir(filepath.Join(path, name), func(_ string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			count++
			if count > localMaxObjects || d.Type()&os.ModeSymlink != 0 {
				return ErrGitRepositoryInvalid
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, ErrGitRepositoryInvalid
		}
	}
	return r, nil
}

func copyLocalObjects(ctx context.Context, source, target *git.Repository, start, stop plumbing.Hash, targetRoot string) error {
	seen := make(map[plumbing.Hash]bool)
	queue := []plumbing.Hash{start}
	var bytes int64
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		hash := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[hash] || hash == stop {
			continue
		}
		seen[hash] = true
		if len(seen) > localMaxObjects {
			return ErrGitRepositoryInvalid
		}
		alreadyPresent := target.Storer.HasEncodedObject(hash) == nil
		obj, err := source.Storer.EncodedObject(plumbing.AnyObject, hash)
		if err != nil {
			return sanitizeGitError(err)
		}
		if !alreadyPresent {
			bytes += obj.Size()
		}
		if obj.Size() < 0 || bytes > localMaxBytes || obj.Size() > 100<<20 || (obj.Type() != plumbing.BlobObject && obj.Size() > 32<<20) {
			return ErrGitRepositoryInvalid
		}
		decoded, err := object.DecodeObject(source.Storer, obj)
		if err != nil {
			return sanitizeGitError(err)
		}
		switch v := decoded.(type) {
		case *object.Commit:
			queue = append(queue, v.TreeHash)
			queue = append(queue, v.ParentHashes...)
		case *object.Tree:
			if len(queue)+len(v.Entries) > localMaxObjects {
				return ErrGitRepositoryInvalid
			}
			for _, entry := range v.Entries {
				if entry.Mode == filemode.Submodule {
					return ErrGitRepositoryInvalid
				}
				queue = append(queue, entry.Hash)
			}
		case *object.Tag:
			queue = append(queue, v.Target)
		}
		if alreadyPresent {
			continue
		}
		wrapped := &contextGitObject{EncodedObject: obj, ctx: ctx}
		got, err := target.Storer.SetEncodedObject(wrapped)
		if err != nil {
			return errors.Join(ErrRepositoryUnavailable, ctx.Err())
		}
		if got != hash || wrapped.hasher.Sum() != hash {
			return ErrGitRepositoryInvalid
		}
		// Flush the installed loose object before exposing a branch that names it.
		objectPath := filepath.Join(targetRoot, "objects", hash.String()[:2], hash.String()[2:])
		file, err := os.Open(objectPath)
		if err != nil {
			return ErrRepositoryUnavailable
		}
		err = file.Sync()
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return ErrRepositoryUnavailable
		}
		if syncRepositoryDirectory(filepath.Dir(objectPath)) != nil || syncRepositoryDirectory(filepath.Join(targetRoot, "objects")) != nil {
			return ErrRepositoryUnavailable
		}
	}
	return nil
}

type contextGitObject struct {
	plumbing.EncodedObject
	ctx    context.Context
	hasher plumbing.Hasher
}

func (o *contextGitObject) Reader() (io.ReadCloser, error) {
	r, err := o.EncodedObject.Reader()
	if err != nil {
		return nil, err
	}
	o.hasher = plumbing.NewHasher(o.Type(), o.Size())
	return &contextGitReader{ReadCloser: r, ctx: o.ctx, hasher: &o.hasher}, nil
}

type contextGitReader struct {
	io.ReadCloser
	ctx    context.Context
	hasher *plumbing.Hasher
}

func (r *contextGitReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.ReadCloser.Read(p)
	r.hasher.Write(p[:n])
	return n, err
}

// publishLocal holds Git's exclusive branch lock while checking the old head,
// copying durable objects, and atomically renaming exactly one reference.
func publishLocal(ctx context.Context, source *git.Repository, a RepositoryAccess, p PreparedPublication) (PublishResult, error) {
	target, err := openLocalRepositoryContext(ctx, a.PublishURL)
	if err != nil {
		return PublishResult{}, err
	}
	targetRoot, err := nativeRepositoryPath(a.PublishURL)
	if err != nil {
		return PublishResult{}, err
	}
	branch := plumbing.NewBranchReferenceName(a.Branch)
	path := filepath.Join(targetRoot, filepath.FromSlash(branch.String()))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return PublishResult{}, ErrRepositoryUnavailable
	}
	mode := os.FileMode(0644)
	if info, err := os.Lstat(path); err == nil {
		mode = info.Mode().Perm() & 0666
	}
	lock, err := os.OpenFile(path+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return PublishResult{}, ErrRepositoryUnavailable
	}
	defer func() { lock.Close(); os.Remove(path + ".lock") }()
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	current, err := target.Reference(branch, true)
	if err != nil || current.Hash().String() != p.ExpectedRemoteRevision {
		return PublishResult{}, ErrRemoteRevisionChanged
	}
	commit := plumbing.NewHash(p.CommitID)
	ff, err := commitReachable(source, current.Hash(), commit, 10000)
	if err != nil || !ff {
		return PublishResult{}, ErrRemoteRevisionChanged
	}
	if err := copyLocalObjects(ctx, source, target, commit, current.Hash(), targetRoot); err != nil {
		return PublishResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	if !a.ExpiresAt.After(time.Now()) {
		return PublishResult{}, ErrAuthorization
	}
	if _, err := io.WriteString(lock, commit.String()+"\n"); err != nil {
		return PublishResult{}, ErrRepositoryUnavailable
	}
	if err := lock.Sync(); err != nil {
		return PublishResult{}, ErrRepositoryUnavailable
	}
	if err := lock.Close(); err != nil {
		return PublishResult{}, ErrRepositoryUnavailable
	}
	if err := renameRepositoryReference(path+".lock", path); err != nil {
		return PublishResult{}, ErrRepositoryUnavailable
	}
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		if syncRepositoryDirectory(directory) != nil {
			return PublishResult{Uncertain: true}, ErrRepositoryUnavailable
		}
		if directory == targetRoot {
			break
		}
	}
	return PublishResult{RemoteRevision: p.CommitID, Landed: true}, nil
}
