package serve

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type SourceKind string

const (
	SourceFile      SourceKind = "file"
	SourceDirectory SourceKind = "directory"
)

var (
	ErrInvalidSource = errors.New("invalid serve source")
	ErrSourceChanged = errors.New("serve source changed")
)

type serveOperationError struct {
	message string
	cause   error
}

func (e serveOperationError) Error() string { return e.message }
func (e serveOperationError) Unwrap() error { return e.cause }

func serveOperationFailure(message string, causes ...error) error {
	cause := errors.Join(causes...)
	if cause == nil {
		return nil
	}
	return serveOperationError{message: message, cause: cause}
}

// Source is a canonical, identity-pinned local file or directory.
type Source struct {
	Path string
	Kind SourceKind
	info os.FileInfo
}

func (s Source) Identity() (string, error) {
	if err := s.Revalidate(); err != nil {
		return "", err
	}
	return sourceIdentity(s.info), nil
}

func ResolvePinnedSource(path string, kind SourceKind, identity string) (Source, error) {
	source, err := ResolveSource(path)
	if err != nil {
		return Source{}, err
	}
	actual, err := source.Identity()
	if err != nil || source.Kind != kind || identity == "" || actual != identity {
		return Source{}, serveOperationFailure("pinned serve source changed or became unavailable", ErrSourceChanged, err)
	}
	return source, nil
}

func ResolveSource(path string) (Source, error) {
	if path == "" {
		return Source{}, ErrInvalidSource
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Source{}, serveOperationFailure("could not resolve selected serve source", ErrInvalidSource, err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return Source{}, serveOperationFailure("could not resolve selected serve source", ErrInvalidSource, err)
	}
	canonical = filepath.Clean(canonical)
	info, err := os.Stat(canonical)
	if err != nil {
		return Source{}, serveOperationFailure("could not inspect selected serve source", ErrInvalidSource, err)
	}
	kind := SourceFile
	if info.IsDir() {
		kind = SourceDirectory
	} else if !info.Mode().IsRegular() {
		return Source{}, fmt.Errorf("serve source must be a regular file or directory: %w", ErrInvalidSource)
	}
	return Source{Path: canonical, Kind: kind, info: info}, nil
}

func (s Source) Revalidate() error {
	if !filepath.IsAbs(s.Path) || s.info == nil || s.Kind != SourceFile && s.Kind != SourceDirectory {
		return ErrInvalidSource
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		return serveOperationFailure("selected serve source changed or became unavailable", ErrSourceChanged, err)
	}
	if !os.SameFile(s.info, info) || s.Kind == SourceFile && !info.Mode().IsRegular() || s.Kind == SourceDirectory && !info.IsDir() {
		return ErrSourceChanged
	}
	return nil
}
