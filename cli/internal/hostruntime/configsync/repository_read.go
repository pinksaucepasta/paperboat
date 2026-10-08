package configsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// RepositoryReadAccess authorizes only an account's registered repository read.
// It deliberately has no publication target, assignment or machine identity.
type RepositoryReadAccess struct {
	RepositoryID string    `json:"repository_id"`
	CloneURL     string    `json:"clone_url"`
	Branch       string    `json:"branch"`
	Transport    string    `json:"transport"`
	Username     string    `json:"username"`
	Password     string    `json:"password"`
	Capability   string    `json:"capability"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (RepositoryReadAccess) String() string     { return "repository read access (redacted)" }
func (a RepositoryReadAccess) GoString() string { return a.String() }

// Bootstrap repositories are configuration-sized: unrelated repository payload
// is limited to 64 MiB, 10,000 objects and 16 MiB per inflated object. No working
// tree, hooks, scripts, publication or mapped destination is opened.
const repositoryReadMaxBytes int64 = 64 << 20
const repositoryReadMaxObjectBytes int64 = 16 << 20
const repositoryReadMaxObjects = 10000

var ErrRepositoryReadLimit = errors.New("repository bootstrap exceeds read limits")

func ReadSharedRepositoryConfig(ctx context.Context, root, credentialRoot string, access RepositoryReadAccess, limits SourceConfigLimits) (SourceConfig, string, error) {
	return readSharedRepositoryConfig(ctx, root, credentialRoot, access, limits, false)
}

// ReadSharedAuthorizedRepositoryConfig consumes an actual machine read/write
// grant; publication remains unavailable through this read-only operation.
func ReadSharedAuthorizedRepositoryConfig(ctx context.Context, root, credentialRoot string, access RepositoryAccess, limits SourceConfigLimits) (SourceConfig, string, error) {
	if access.AssignmentID == "" || access.EnvironmentID == "" || access.MachineID == "" {
		return SourceConfig{}, "", ErrAuthorization
	}
	return readSharedRepositoryConfig(ctx, root, credentialRoot, RepositoryReadAccess{RepositoryID: access.RepositoryID, CloneURL: access.CloneURL, Branch: access.Branch, Transport: access.Transport, Username: access.Username, Password: access.Password, Capability: access.Capability, ExpiresAt: access.ExpiresAt}, limits, true)
}
func readSharedRepositoryConfig(ctx context.Context, root, credentialRoot string, access RepositoryReadAccess, limits SourceConfigLimits, machineGrant bool) (source SourceConfig, revision string, resultErr error) {
	fail := func(err error) (SourceConfig, string, error) { return SourceConfig{}, "", err }
	normalized, kind, err := NormalizeRepositoryEndpoint(access.CloneURL)
	if err != nil || normalized != access.CloneURL || kind != access.Transport || access.RepositoryID == "" || access.Branch == "" || plumbing.NewBranchReferenceName(access.Branch).Validate() != nil || (access.Capability != "repository_contents_read" && (!machineGrant || access.Capability != "repository_contents_write")) {
		return fail(ErrAuthorization)
	}
	if !access.ExpiresAt.After(time.Now()) {
		return fail(ErrAuthorization)
	}
	defaults := DefaultSourceConfigLimits()
	if limits.MaxBytes <= 0 || limits.MaxBytes > defaults.MaxBytes || limits.MaxPaths <= 0 || limits.MaxPaths > defaults.MaxPaths || limits.MaxPatterns <= 0 || limits.MaxPatterns > defaults.MaxPatterns || limits.MaxPatternBytes <= 0 || limits.MaxPatternBytes > defaults.MaxPatternBytes {
		return fail(ErrSourceConfigInvalid)
	}
	ctx, cancel := context.WithDeadline(ctx, access.ExpiresAt)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		return fail(repositoryUnavailableFailure(err))
	}
	if kind == "local" {
		if _, err := openLocalRepositoryContext(ctx, access.CloneURL); err != nil {
			return fail(err)
		}
	}
	opts, err := resolveRepositoryTransportAccess(ctx, credentialRoot, RepositoryAccess{RepositoryID: access.RepositoryID, CloneURL: access.CloneURL, Transport: access.Transport, Username: access.Username, Password: access.Password})
	if err != nil {
		return fail(err)
	}
	defer func() {
		if opts.closer != nil {
			if closeErr := opts.closer.Close(); closeErr != nil {
				resultErr = repositoryCleanupFailure(resultErr, closeErr)
				source = SourceConfig{}
				revision = ""
			}
		}
	}()
	initRepositoryTransports()
	staging, err := newPrivateRepositoryTemporaryDirectory(root, ".repository-read-")
	if err != nil {
		return fail(err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(staging); cleanupErr != nil {
			resultErr = repositoryCleanupFailure(resultErr, cleanupErr)
			source = SourceConfig{}
			revision = ""
		}
	}()
	if err := EnsureRepositoryCredentialRoot(staging); err != nil {
		return fail(repositoryUnavailableFailure(err))
	}
	store := &repositoryReadStorage{Storer: filesystem.NewStorage(osfs.New(filepath.Clean(staging)), cache.NewObjectLRUDefault()), ctx: ctx, root: staging}
	repository, err := git.CloneContext(ctx, store, nil, &git.CloneOptions{URL: access.CloneURL, Auth: opts.auth, CABundle: opts.ca, RemoteName: "origin", ReferenceName: plumbing.NewBranchReferenceName(access.Branch), SingleBranch: true, NoCheckout: true, Tags: git.NoTags})
	if err != nil {
		underlying := gitUnderlyingError(err)
		if errors.Is(underlying, ErrRepositoryReadLimit) && !errors.Is(underlying, ErrRepositoryUnavailable) {
			return fail(ErrRepositoryReadLimit)
		}
		return fail(sanitizeGitError(err))
	}
	ref, err := repository.Reference(plumbing.NewBranchReferenceName(access.Branch), true)
	if err != nil {
		return fail(sanitizeGitError(err))
	}
	if ref.Hash().IsZero() {
		return fail(ErrGitRepositoryInvalid)
	}
	revision = ref.Hash().String()
	commit, err := repository.CommitObject(ref.Hash())
	if err != nil {
		return fail(sanitizeGitError(err))
	}
	tree, err := commit.Tree()
	if err != nil {
		return fail(sanitizeGitError(err))
	}
	file, err := tree.File(SharedSourceConfigPath)
	if errors.Is(err, object.ErrFileNotFound) {
		return SourceConfig{}, revision, nil
	}
	if err != nil {
		return fail(sanitizeGitError(err))
	}
	if !file.Mode.IsRegular() || file.Size > int64(limits.MaxBytes) {
		return fail(ErrSourceConfigInvalid)
	}
	reader, err := file.Reader()
	if err != nil {
		return fail(sanitizeGitError(err))
	}
	data, err := io.ReadAll(io.LimitReader(&repositoryContextReader{ctx: ctx, reader: reader}, int64(limits.MaxBytes)+1))
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		clear(data)
		if err != nil && closeErr != nil {
			return fail(repositoryCleanupFailure(sanitizeGitError(err), closeErr))
		}
		if err != nil {
			return fail(sanitizeGitError(err))
		}
		return fail(repositoryUnavailableFailure(closeErr))
	}
	defer clear(data)
	if len(data) > limits.MaxBytes {
		return fail(ErrSourceConfigInvalid)
	}
	source, err = ParseSourceConfig(data, limits)
	if err != nil {
		return fail(err)
	}
	return source, revision, nil
}

func repositoryCleanupFailure(previous, cleanup error) error {
	if cleanup == nil {
		return previous
	}
	return repositoryUnavailableFailure(errors.Join(previous, cleanup))
}

type repositoryContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *repositoryContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// The bounded raw pack is validated before go-git resolves deltas. Both
// delta instruction lengths and declared expanded target sizes are checked.
type repositoryReadStorage struct {
	storage.Storer
	ctx  context.Context
	root string
}

func (s *repositoryReadStorage) PackfileWriter() (io.WriteCloser, error) {
	file, err := os.CreateTemp(s.root, ".bounded-pack-")
	if err != nil {
		return nil, err
	}
	return &repositoryReadPackWriter{file: file, ctx: s.ctx, store: s.Storer}, nil
}

type repositoryReadPackWriter struct {
	file     *os.File
	store    storage.Storer
	ctx      context.Context
	bytes    int64
	writeErr error
}

func (w *repositoryReadPackWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if err := w.ctx.Err(); err != nil {
		w.writeErr = err
		return 0, err
	}
	w.bytes += int64(len(p))
	if w.bytes > repositoryReadMaxBytes {
		w.writeErr = ErrRepositoryReadLimit
		return 0, w.writeErr
	}
	n, err := w.file.Write(p)
	w.writeErr = err
	return n, err
}
func (w *repositoryReadPackWriter) Close() (resultErr error) {
	defer func() {
		closeErr := w.file.Close()
		removeErr := os.Remove(w.file.Name())
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		if closeErr != nil {
			resultErr = repositoryCleanupFailure(resultErr, closeErr)
		}
		if removeErr != nil {
			resultErr = repositoryCleanupFailure(resultErr, removeErr)
		}
	}()
	if w.writeErr != nil {
		return w.writeErr
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	scanner := packfile.NewScanner(w.file)
	_, count, err := scanner.Header()
	if err != nil {
		return err
	}
	observer := &repositoryReadObserver{}
	if err = observer.OnHeader(count); err != nil {
		return err
	}
	for range count {
		if err = w.ctx.Err(); err != nil {
			return err
		}
		header, err := scanner.NextObjectHeader()
		if err != nil {
			return err
		}
		if header.Length < 0 || header.Length > repositoryReadMaxObjectBytes {
			return ErrRepositoryReadLimit
		}
		prefix := &repositoryDeltaPrefix{}
		var sink io.Writer = io.Discard
		if header.Type.IsDelta() {
			sink = prefix
		}
		if _, _, err = scanner.NextObject(sink); err != nil {
			return err
		}
		expanded := header.Length
		if header.Type.IsDelta() {
			reader := bytes.NewReader(prefix.data)
			base, err := binary.ReadUvarint(reader)
			if err != nil || base > uint64(repositoryReadMaxObjectBytes) {
				return ErrRepositoryReadLimit
			}
			target, err := binary.ReadUvarint(reader)
			if err != nil || target > uint64(repositoryReadMaxObjectBytes) {
				return ErrRepositoryReadLimit
			}
			expanded = int64(target)
		}
		if err = observer.OnInflatedObjectHeader(header.Type, expanded, header.Offset); err != nil {
			return err
		}
	}
	if _, err = w.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	parser, err := packfile.NewParserWithStorage(packfile.NewScanner(&repositoryContextReadSeeker{file: w.file, ctx: w.ctx}), w.store)
	if err != nil {
		return err
	}
	_, err = parser.Parse()
	return err
}

type repositoryDeltaPrefix struct{ data []byte }

func (p *repositoryDeltaPrefix) Write(data []byte) (int, error) {
	n := len(data)
	if remaining := 20 - len(p.data); remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		p.data = append(p.data, data...)
	}
	return n, nil
}

type repositoryReadObserver struct{ bytes int64 }

func (o *repositoryReadObserver) OnHeader(count uint32) error {
	if count > repositoryReadMaxObjects {
		return ErrRepositoryReadLimit
	}
	return nil
}
func (o *repositoryReadObserver) OnInflatedObjectHeader(_ plumbing.ObjectType, size, pos int64) error {
	o.bytes += size
	if size < 0 || size > repositoryReadMaxObjectBytes || o.bytes > repositoryReadMaxBytes {
		return ErrRepositoryReadLimit
	}
	return nil
}
func (*repositoryReadObserver) OnInflatedObjectContent(plumbing.Hash, int64, uint32, []byte) error {
	return nil
}
func (*repositoryReadObserver) OnFooter(plumbing.Hash) error { return nil }

type repositoryContextReadSeeker struct {
	file *os.File
	ctx  context.Context
}

func (r *repositoryContextReadSeeker) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.Read(p)
}
func (r *repositoryContextReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.Seek(offset, whence)
}
