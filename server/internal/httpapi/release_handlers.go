package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/machinealias"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var selfhostPackagePathPattern = regexp.MustCompile(`^/selfhost/versions/[0-9.]+/paperboat-selfhost-linux-(amd64|arm64)\.tar\.gz$`)
var enrollmentTokenPattern = regexp.MustCompile(`^[0-9A-Z]{26}$`)

func NewReleaseFiles(directory string) (http.Handler, error) {
	directory = filepath.Clean(strings.TrimSpace(directory))
	info, err := os.Lstat(directory)
	if err != nil || !filepath.IsAbs(directory) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, os.ErrInvalid
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relative := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(r.URL.Path)), "/")
		if relative != "install" && relative != "windows" && !strings.HasPrefix(relative, "tuf/metadata/") && relative != "selfhost/install" && !strings.HasPrefix(relative, "selfhost/versions/") {
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

func installScript(files http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Vary", "User-Agent")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// A plain installation selects the shell from its HTTP client. An
		// enrollment parameter also carries the shell choice in its token tag.
		powershell := strings.Contains(strings.ToLower(r.UserAgent()), "powershell")
		path := "/install"
		if powershell {
			path = "/windows"
		}
		serveEnrollmentScript(w, r, files, path, powershell)
	}
}

func enrollmentTokenUsesPowerShell(token string) bool {
	return len(token) == 26 && strings.Contains("13579ACEGIKMOQSUWY", token[:1])
}

func serveEnrollmentScript(w http.ResponseWriter, r *http.Request, files http.Handler, path string, powershell bool) {
	plain := r.URL.RawQuery == ""
	token, hostname := "", ""
	if !plain {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query) != 1 || len(query["p"]) != 1 {
			http.Error(w, "invalid enrollment parameter", http.StatusBadRequest)
			return
		}
		var ok bool
		token, hostname, ok = splitEnrollmentParameter(query["p"][0])
		if !ok {
			http.Error(w, "invalid enrollment parameter", http.StatusBadRequest)
			return
		}
	}
	powershell = powershell || enrollmentTokenUsesPowerShell(token)
	if powershell {
		path = "/windows"
	}
	recorder := httptest.NewRecorder()
	clone := r.Clone(r.Context())
	clone.URL.Path = path
	files.ServeHTTP(recorder, clone)
	if recorder.Code != http.StatusOK {
		http.Error(w, "installer unavailable", recorder.Code)
		return
	}
	if plain {
		body := recorder.Body.Bytes()
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
		return
	}
	preamble := []byte("$env:PAPERBOAT_ENROLLMENT_TOKEN='" + strings.ReplaceAll(token, "'", "''") + "';")
	if powershell {
		if hostname != "" {
			preamble = append(preamble, []byte("$env:PAPERBOAT_MACHINE_ALIAS='"+hostname+"';")...)
		}
	} else {
		// Keep the installer's shebang as the first line. POSIX shells interpret
		// a bare assignment-only line as a command that exits successfully, so
		// prefixing the script made `curl ... | bash` silently do nothing.
		script := bytes.TrimPrefix(recorder.Body.Bytes(), []byte("\xef\xbb\xbf"))
		lineEnd := bytes.IndexByte(script, '\n')
		if lineEnd < 0 || !bytes.Equal(script[:lineEnd], []byte("#!/bin/sh")) {
			http.Error(w, "installer unavailable", http.StatusInternalServerError)
			return
		}
		preamble = append([]byte{}, script[:lineEnd+1]...)
		preamble = append(preamble, []byte("PAPERBOAT_ENROLLMENT_TOKEN='"+token+"' PAPERBOAT_MACHINE_ALIAS='"+hostname+"'\nexport PAPERBOAT_ENROLLMENT_TOKEN PAPERBOAT_MACHINE_ALIAS\n")...)
		recorder.Body.Reset()
		_, _ = recorder.Body.Write(script[lineEnd+1:])
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	body := append(preamble, bytes.TrimPrefix(recorder.Body.Bytes(), []byte("\xef\xbb\xbf"))...)
	// PowerShell 5.1 can mishandle a chunked text/plain installer response.
	// Advertise the exact size so the dashboard's direct iwr-to-file command is
	// deterministic in the user's current PowerShell process.
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

func splitEnrollmentParameter(value string) (string, string, bool) {
	value = strings.TrimSpace(value)
	if len(value) == 26 && enrollmentTokenPattern.MatchString(value) {
		return value, "", true
	}
	if len(value) <= 27 {
		return "", "", false
	}
	separator := len(value) - 27
	if value[separator] != '-' {
		return "", "", false
	}
	hostname, token := value[:separator], value[separator+1:]
	if !machinealias.Valid(hostname) || !enrollmentTokenPattern.MatchString(token) {
		return "", "", false
	}
	return token, hostname, true
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

// Self-host installers are scope-agnostic and never accept machine enrollment tokens.
func selfhostInstaller(files http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
}
func selfhostPackage(files http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !selfhostPackagePathPattern.MatchString(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
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
