package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func NewReleaseFiles(directory string) (http.Handler, error) {
	directory = filepath.Clean(strings.TrimSpace(directory))
	info, err := os.Lstat(directory)
	if err != nil || !filepath.IsAbs(directory) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, os.ErrInvalid
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relative := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(r.URL.Path)), "/")
		if !strings.HasPrefix(relative, "tuf/metadata/") {
			http.NotFound(w, r)
			return
		}
		file, info, openErr := openRegularFile(directory, relative)
		if openErr != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		http.ServeContent(w, r, filepath.Base(relative), info.ModTime(), file)
	}), nil
}

func openRegularFile(root, relative string) (*os.File, os.FileInfo, error) {
	current := root
	for _, component := range strings.Split(filepath.FromSlash(relative), string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return nil, nil, os.ErrInvalid
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, os.ErrInvalid
		}
	}
	file, err := os.Open(current)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, errors.New("release object is not a regular file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func tufRepository(prefix string, files http.Handler) http.Handler {
	return http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metadata := strings.HasPrefix(r.URL.Path, "/metadata/") && strings.HasSuffix(r.URL.Path, ".json") && !strings.HasSuffix(r.URL.Path, "/")
		if !metadata || strings.Contains(r.URL.Path, "/.") {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/timestamp.json") {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		clone := r.Clone(r.Context())
		clone.URL.Path = "/tuf" + r.URL.Path
		files.ServeHTTP(w, clone)
	}))
}

func selfhostAddress(w http.ResponseWriter, r *http.Request) {
	address := resolvedRequestNetwork(r)
	ip := net.ParseIP(address)
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
		http.Error(w, "public address unavailable; use --endpoint-host", http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"address": address})
}
