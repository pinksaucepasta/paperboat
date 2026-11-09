package managedssh

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// remoteLoginGroup is the macOS service ACL group of Remote Login ("Allow access for: Only these users").
// When the group does not exist every user may log in; the admin group is nested in it by default.
const remoteLoginGroup = "com.apple.access_ssh"

// ErrSSHLoginNotAllowed is a stream the host refused because the machine's sshd would refuse the managed user.
var ErrSSHLoginNotAllowed = errors.New("the machine's Remote Login does not allow this user to log in over SSH")

// ExitRunner runs a command and returns its exit code; an error means it could not run.
type ExitRunner func(name string, args ...string) (int, error)

// RemoteLoginDenied is true when macOS Remote Login is restricted to chosen users and user is not one of them.
// Anything it cannot establish counts as allowed: sshd stays the authority, this only explains its refusal.
func RemoteLoginDenied(user string, run ExitRunner) bool {
	if run == nil || strings.TrimSpace(user) == "" || strings.HasPrefix(user, "-") {
		return false
	}
	if code, err := run("dscl", ".", "-read", "/Groups/"+remoteLoginGroup); err != nil || code != 0 {
		return false
	}
	code, err := run("dseditgroup", "-o", "checkmember", "-m", user, remoteLoginGroup)
	return err == nil && code != 0
}

// loginDenied caches the answer per user: the ACL changes rarely and every SSH stream asks.
type loginDenied struct {
	mu      sync.Mutex
	entries map[string]loginDeniedEntry
}

type loginDeniedEntry struct {
	checked time.Time
	denied  bool
}

const (
	loginDeniedCacheFor = 15 * time.Second
	loginDeniedMaxUsers = 64
)

func (c *loginDenied) get(user string, check func(string) bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[user]; ok && time.Since(entry.checked) <= loginDeniedCacheFor {
		return entry.denied
	}
	if c.entries == nil || len(c.entries) >= loginDeniedMaxUsers {
		c.entries = make(map[string]loginDeniedEntry)
	}
	denied := check(user)
	c.entries[user] = loginDeniedEntry{checked: time.Now(), denied: denied}
	return denied
}

type loginUserKey struct{}

// WithLoginUser names the OS user a stream will log in as; the stream's header carries it.
func WithLoginUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, loginUserKey{}, user)
}

func loginUserFrom(ctx context.Context) string {
	user, _ := ctx.Value(loginUserKey{}).(string)
	return user
}
