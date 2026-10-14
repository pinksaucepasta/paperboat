// Package privateproxyconfig owns the reversible host proxy configuration used
// to route Paperboat private HTTP names through hostd.
package privateproxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

var (
	ErrUnsupported  = errors.New("private proxy configuration is unsupported")
	ErrConflict     = errors.New("proxy configuration is no longer owned by Paperboat")
	ErrUntrustedPAC = errors.New("PAC URL is not a trusted Paperboat loopback URL")
)

// Adapter snapshots and restores the exact platform-owned proxy fields.
// Snapshot values must be JSON and are opaque to Manager.
type Adapter interface {
	Name() string
	Snapshot(context.Context) (json.RawMessage, error)
	Install(context.Context, string) error
	Owns(context.Context, string) (bool, error)
	// OwnsTransition accepts only fields containing either exact owned URL.
	OwnsTransition(context.Context, string, string) (bool, error)
	// OwnsRestoration accepts only prior values and this transaction's PAC values.
	OwnsRestoration(context.Context, json.RawMessage, string, string) (bool, error)
	Matches(context.Context, json.RawMessage) (bool, error)
	Restore(context.Context, json.RawMessage) error
}

type Manager struct {
	journalPath string
	adapter     Adapter
	mu          sync.Mutex
}

func New(journalPath string, adapter Adapter) (*Manager, error) {
	if !filepath.IsAbs(journalPath) || adapter == nil || adapter.Name() == "" {
		return nil, errors.New("private proxy configuration requires an absolute journal path and adapter")
	}
	return &Manager{journalPath: filepath.Clean(journalPath), adapter: adapter}, nil
}

type journal struct {
	Version        int             `json:"version"`
	Adapter        string          `json:"adapter"`
	PACURL         string          `json:"pac_url"`
	PreviousPACURL string          `json:"previous_pac_url,omitempty"`
	Phase          string          `json:"phase"`
	Prior          json.RawMessage `json:"prior"`
}

// Install records recoverable pre-state before changing the operating system.
func (m *Manager) Install(ctx context.Context, pacURL string) (resultErr error) {
	defer protectProxyResult(&resultErr)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := proxyContextError(ctx); err != nil {
		return err
	}
	if err := validatePACURL(pacURL); err != nil {
		return err
	}
	if existing, err := m.read(); err == nil {
		if existing.Adapter != m.adapter.Name() || existing.PACURL != pacURL {
			return ErrConflict
		}

		owned, err := m.ownsJournal(ctx, existing)
		if err != nil {
			return err
		}
		if existing.Phase == "applied" {
			if owned {
				return nil
			}
			return ErrConflict
		}
		// An interrupted first install may contain captured prior/PAC mixtures.
		// Reconcile them before attempting a fresh transaction; other changes
		// remain an ownership conflict.
		if existing.Phase != "prepared" && existing.Phase != "restoring" {
			return ErrConflict
		}
		if !owned {
			matches, err := m.adapter.Matches(ctx, existing.Prior)
			if err != nil {
				return err
			}
			if !matches {
				return ErrConflict
			}
			if err := m.removeJournal(); err != nil {
				return err
			}
		} else if err := m.restoreJournal(ctx, existing); err != nil {
			return err
		}
	} else if !proxyMissingOnly(err) {
		return err
	}

	prior, err := m.adapter.Snapshot(ctx)
	if err != nil {
		return err
	}
	j := journal{Version: 1, Adapter: m.adapter.Name(), PACURL: pacURL, Phase: "prepared", Prior: prior}
	if err := m.write(j); err != nil {
		return err
	}

	if err := m.adapter.Install(ctx, pacURL); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		owned, ownershipErr := m.ownsJournal(cleanup, j)
		if ownershipErr != nil || !owned {
			return errors.Join(err, ownershipErr, ErrConflict)
		}
		return errors.Join(err, m.restoreJournal(cleanup, j))
	}
	j.Phase = "applied"
	if err := m.write(j); err != nil {
		return err
	}
	return nil
}

// Remove restores the exact pre-install state. It never overwrites a setting
// changed by the user or another program after Paperboat installed its PAC.
func (m *Manager) Remove(ctx context.Context) (resultErr error) {
	defer protectProxyResult(&resultErr)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := proxyContextError(ctx); err != nil {
		return err
	}
	j, err := m.read()
	if proxyMissingOnly(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.Adapter != m.adapter.Name() {
		return ErrConflict
	}
	owned, err := m.ownsJournal(ctx, j)
	if err != nil {
		return err
	}
	if !owned {
		matches, matchErr := m.adapter.Matches(ctx, j.Prior)
		if matchErr != nil {
			return matchErr
		}
		// Restore may already have completed before the process stopped. Once
		// the platform state exactly matches the captured prior state, the
		// journal is stale regardless of whether the last durable phase was
		// prepared or applied.
		if matches {
			return m.removeJournal()
		}
		return ErrConflict
	}
	return m.restoreJournal(ctx, j)
}

// Recover rolls back an interrupted install. Call it before the first Install.
func (m *Manager) Recover(ctx context.Context) (resultErr error) {
	defer protectProxyResult(&resultErr)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := proxyContextError(ctx); err != nil {
		return err
	}
	j, err := m.read()
	if proxyMissingOnly(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.Adapter != m.adapter.Name() {
		return ErrConflict
	}
	owned, err := m.ownsJournal(ctx, j)
	if err != nil {
		return err
	}
	if !owned {
		matches, matchErr := m.adapter.Matches(ctx, j.Prior)
		if matchErr != nil {
			return matchErr
		}
		if matches {
			return m.removeJournal()
		}
		return ErrConflict
	}
	return m.restoreJournal(ctx, j)
}

func validatePACURL(value string) error {
	u, err := url.Parse(value)
	if err != nil {
		return proxyFailure(errors.Join(ErrUntrustedPAC, err))
	}
	if u.Scheme != "http" || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return ErrUntrustedPAC
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() || u.Port() == "" || u.Path == "" || u.Path == "/" {
		return ErrUntrustedPAC
	}
	return nil
}

func (m *Manager) write(j journal) error {
	body, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return atomicfile.Write(m.journalPath, append(body, '\n'), atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}
func (m *Manager) read() (journal, error) {
	info, err := os.Lstat(m.journalPath)
	if err != nil {
		return journal{}, err
	}
	if !info.Mode().IsRegular() {
		return journal{}, errors.New("private proxy journal must be a private regular file")
	}
	body, err := readProxyJournal(m.journalPath, info)
	if err != nil {
		return journal{}, err
	}
	var j journal
	if err := json.Unmarshal(body, &j); err != nil {
		return journal{}, proxyFailure(err)
	}
	if j.Version != 1 || j.Adapter == "" || j.PACURL == "" || (j.Phase != "prepared" && j.Phase != "applied" && j.Phase != "refreshing" && j.Phase != "restoring") || len(j.Prior) == 0 {
		return journal{}, errors.New("invalid private proxy journal")
	}
	if err := validatePACURL(j.PACURL); err != nil {
		return journal{}, err
	}
	if j.Phase == "refreshing" {
		if err := validatePACURL(j.PreviousPACURL); err != nil {
			return journal{}, err
		}
	} else if j.Phase == "restoring" && j.PreviousPACURL != "" {
		if err := validatePACURL(j.PreviousPACURL); err != nil {
			return journal{}, err
		}
	} else if j.PreviousPACURL != "" {
		return journal{}, errors.New("invalid proxy refresh journal")
	}
	return j, nil
}
func (m *Manager) removeJournal() error {
	if err := os.Remove(m.journalPath); err != nil && !proxyMissingOnly(err) {
		return err
	}
	return syncProxyDirectory(filepath.Dir(m.journalPath))
}

func (m *Manager) ownsJournal(ctx context.Context, j journal) (bool, error) {
	if j.Phase == "restoring" || j.Phase == "prepared" {
		return m.adapter.OwnsRestoration(ctx, j.Prior, j.PACURL, j.PreviousPACURL)
	}
	if j.Phase == "refreshing" {
		return m.adapter.OwnsTransition(ctx, j.PreviousPACURL, j.PACURL)
	}
	return m.adapter.Owns(ctx, j.PACURL)
}

// Refresh changes only this transaction's owned PAC URL. The original user
// snapshot survives every revision and is restored by Remove or Recover.
func (m *Manager) Refresh(ctx context.Context, pacURL string) (resultErr error) {
	defer protectProxyResult(&resultErr)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := proxyContextError(ctx); err != nil {
		return err
	}
	if err := validatePACURL(pacURL); err != nil {
		return err
	}
	j, err := m.read()
	if err != nil {
		return err
	}
	if j.Adapter != m.adapter.Name() {
		return ErrConflict
	}
	if j.Phase == "refreshing" {
		owned, err := m.ownsJournal(ctx, j)
		if err != nil {
			return err
		}
		if !owned {
			return ErrConflict
		}
		// An interrupted refresh is completed only for the recorded candidate.
		// Otherwise restore the last installed Paperboat URL before starting anew.
		target := j.PreviousPACURL
		if pacURL == j.PACURL {
			target = j.PACURL
		}
		if err := m.adapter.Install(ctx, target); err != nil {
			return err
		}
		j.PACURL = target
		j.PreviousPACURL = ""
		j.Phase = "applied"
		if err := m.write(j); err != nil {
			return err
		}
	}
	if j.Adapter != m.adapter.Name() || j.Phase != "applied" {
		return ErrConflict
	}
	owned, err := m.adapter.Owns(ctx, j.PACURL)
	if err != nil {
		return err
	}
	if !owned {
		return ErrConflict
	}
	if pacURL == j.PACURL {
		return nil
	}
	// Revision changes cannot move the trusted listener or its loopback origin.
	oldURL, _ := url.Parse(j.PACURL)
	newURL, _ := url.Parse(pacURL)
	if oldURL.Host != newURL.Host {
		return ErrUntrustedPAC
	}
	priorPAC := j.PACURL
	before, err := m.adapter.Snapshot(ctx)
	if err != nil {
		return err
	}
	j.PreviousPACURL = priorPAC
	j.PACURL = pacURL
	j.Phase = "refreshing"
	if err := m.write(j); err != nil {
		return err
	}
	applyErr := m.adapter.Install(ctx, pacURL)
	if applyErr != nil {
		// Restore even after caller cancellation, but never overwrite an external
		// setting observed after a partial apply. Multi-interface mixtures are
		// owned only when every field is one of these two explicit PAC URLs.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		owned, ownershipErr := m.ownsJournal(cleanup, j)
		if ownershipErr != nil || !owned {
			return errors.Join(applyErr, ownershipErr, ErrConflict)
		}
		if err := m.adapter.Restore(cleanup, before); err != nil {
			return errors.Join(applyErr, err)
		}
		j.PACURL = priorPAC
		j.PreviousPACURL = ""
		j.Phase = "applied"
		return errors.Join(applyErr, m.write(j))
	}
	j.PreviousPACURL = ""
	j.Phase = "applied"
	return m.write(j)
}

// A durable restoring phase records the allowed original/PAC mixtures before
// the first restoration write. Later recovery may continue exactly that work.
func (m *Manager) restoreJournal(ctx context.Context, j journal) error {
	if j.Phase != "restoring" {
		j.Phase = "restoring"
		if err := m.write(j); err != nil {
			return err
		}
	}
	if err := m.adapter.Restore(ctx, j.Prior); err != nil {
		return err
	}
	return m.removeJournal()
}

// Proxy snapshots contain private system settings. Public failures expose only
// owned prose while the final runtime owner can inspect original typed causes.
type proxyConfigFailure struct{ cause error }

func (proxyConfigFailure) Error() string   { return "private proxy configuration failed" }
func (e proxyConfigFailure) Unwrap() error { return e.cause }
func proxyFailure(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(proxyConfigFailure); ok {
		return err
	}
	return proxyConfigFailure{cause: err}
}
func protectProxyResult(err *error) { *err = proxyFailure(*err) }
func proxyContextError(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, context.Cause(ctx))
	}
	return nil
}
func readProxyJournal(path string, expected os.FileInfo) (body []byte, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			body = nil
			resultErr = errors.Join(resultErr, err)
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(expected, opened) || !opened.Mode().IsRegular() || opened.Size() < 0 {
		return nil, errors.New("private proxy journal identity changed")
	}
	if err := validateProxyJournal(file, opened); err != nil {
		return nil, err
	}
	body, err = io.ReadAll(io.LimitReader(file, opened.Size()+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > opened.Size() {
		return nil, errors.New("private proxy journal changed while reading")
	}
	return body, nil
}

// UnsupportedOnly proves that an optional desktop capability is unavailable;
// independent cleanup/I/O failures must still reach the runtime owner.
func UnsupportedOnly(err error) bool {
	return allProxyLeaves(err, func(leaf error) bool { return leaf == ErrUnsupported })
}
func proxyMissingOnly(err error) bool {
	return allProxyLeaves(err, func(leaf error) bool {
		if leaf == os.ErrNotExist {
			return true
		}
		native, ok := leaf.(syscall.Errno)
		return ok && native.Is(os.ErrNotExist)
	})
}
func allProxyLeaves(err error, match func(error) bool) bool {
	pending := []error{err}
	leaves := 0
	for count := 0; len(pending) > 0; count++ {
		if count >= 16 {
			return false
		}
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if node == nil {
			return false
		}
		v := reflect.ValueOf(node)
		if v.Kind() == reflect.Pointer && v.IsNil() {
			return false
		}
		if multi, ok := node.(interface{ Unwrap() []error }); ok {
			children := multi.Unwrap()
			if len(children) == 0 || len(children) > 16-len(pending) {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if unary, ok := node.(interface{ Unwrap() error }); ok {
			pending = append(pending, unary.Unwrap())
			continue
		}
		if !match(node) {
			return false
		}
		leaves++
	}
	return leaves > 0
}
