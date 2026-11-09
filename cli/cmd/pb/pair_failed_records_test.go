package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// failingInstallComputer is one computer and the account it pairs into. The server accepts every pairing
// (a machine record appears in records, and the computer saves its registration, as pb pair does before it
// installs the service); the local installation then ends with the exit code the test asks for.
type failingInstallComputer struct {
	t         *testing.T
	stateRoot string
	records   map[string]bool // the account's machine records on the server
	paired    int
	offline   bool     // the server cannot be reached for the replace call
	replaced  []string // "new<old" replace calls the server accepted
}

func newFailingInstallComputer(t *testing.T) *failingInstallComputer {
	t.Helper()
	isolatedReplacementNote(t)
	computer := &failingInstallComputer{t: t, stateRoot: shorttemp.Dir(t), records: map[string]bool{}}
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", computer.stateRoot)
	originalReplace, originalBootstrap := replaceUserMachine, pairBootstrap
	t.Cleanup(func() { replaceUserMachine, pairBootstrap = originalReplace, originalBootstrap })
	replaceUserMachine = func(_ *command.Context, replacement, retired string) error {
		if computer.offline {
			return errors.New("connection refused")
		}
		if !computer.records[replacement] {
			return errors.New("the replacement is not a machine of this account")
		}
		delete(computer.records, retired)
		computer.replaced = append(computer.replaced, replacement+"<"+retired)
		return nil
	}
	return computer
}

// register is what the server accepting a pairing leaves behind: a record in the account and the
// computer's registration naming it.
func (c *failingInstallComputer) register(id string) {
	c.t.Helper()
	store, err := identity.Open(identity.Config{StateRoot: c.stateRoot})
	if err != nil {
		c.t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{ServerURL: "https://api.example.test", MachineID: id, EnvironmentID: id, PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(c.stateRoot, "inbox"), InstallationGeneration: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		c.t.Fatal(err)
	}
	c.records[id] = true
}

// install is one run of the installer one-liner: pb reset (note the record, wipe the computer's state),
// then pb pair with a new token. It returns the record the server issued and pb pair's error.
func (c *failingInstallComputer) install(installExit int) (string, error) {
	c.t.Helper()
	if _, err := rememberReplacedMachine(); err != nil {
		c.t.Fatal(err)
	}
	if err := os.RemoveAll(c.stateRoot); err != nil {
		c.t.Fatal(err)
	}
	if err := os.MkdirAll(c.stateRoot, 0o700); err != nil {
		c.t.Fatal(err)
	}
	c.paired++
	id := fmt.Sprintf("machine_%d", c.paired)
	pairBootstrap = func(context.Context, []string, io.Reader, io.Writer, io.Writer) int {
		c.register(id)
		return installExit
	}
	command := pairCommand()
	command.PersistentFlags().String("server", "", "") // the root command's flag
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--server", "https://api.example.test", "--enrollment-token-file", freshResetTokenFile(c.t), "--state-root", c.stateRoot})
	return id, command.Execute()
}

// recordList is the account's machine records, sorted (machine_old sorts last).
func (c *failingInstallComputer) recordList() string {
	ids := make([]string, 0, len(c.records))
	for id := range c.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// victus, 2026-11-10: the server accepted each pairing and the local service installation then failed,
// 18 times; every attempt left its machine record in the account (victus-2 … victus-19), because each
// reset noted only the last record and the note forgot the one before. A computer whose pairing keeps
// failing has one record: each new attempt takes over the unfinished one before it.
func TestAPairingThatKeepsFailingLeavesOneMachineRecord(t *testing.T) {
	computer := newFailingInstallComputer(t)
	for attempt := 1; attempt <= 3; attempt++ {
		id, err := computer.install(1)
		if err == nil {
			t.Fatalf("attempt %d: a failed installation reported success", attempt)
		}
		if got := computer.recordList(); got != id {
			t.Fatalf("after failed attempt %d (%v) the account holds %s, want only %s", attempt, err, got, id)
		}
	}
	id, err := computer.install(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := computer.recordList(); got != id {
		t.Fatalf("after the working pairing the account holds %s, want only %s", got, id)
	}
	if left := bootstrap.LoadReplacements(time.Now()); len(left) != 0 {
		t.Fatalf("the note still names %+v after a working pairing", left)
	}
}

// F1-09 stays: a record that worked is replaced only once a pairing works. Failed attempts in between
// take over each other, never the working record, and a replace call that cannot reach the server
// loses nothing: the working pairing replaces every record this computer left, the oldest last (its
// name and local address are the ones the new machine keeps).
func TestFailedPairingsKeepTheWorkingRecordUntilOnePairingWorks(t *testing.T) {
	computer := newFailingInstallComputer(t)
	computer.register("machine_old")
	if _, err := computer.install(1); err == nil {
		t.Fatal("a failed installation reported success")
	}
	if got := computer.recordList(); got != "machine_1,machine_old" || len(computer.replaced) != 0 {
		t.Fatalf("records=%s replaced=%v, want the working record untouched", got, computer.replaced)
	}
	if _, err := computer.install(1); err == nil {
		t.Fatal("a failed installation reported success")
	}
	if got := computer.recordList(); got != "machine_2,machine_old" {
		t.Fatalf("records=%s, want the working record and the latest attempt only", got)
	}
	computer.offline = true
	if _, err := computer.install(1); err == nil {
		t.Fatal("a failed installation reported success")
	}
	if got := computer.recordList(); got != "machine_2,machine_3,machine_old" {
		t.Fatalf("records=%s while the server is unreachable", got)
	}
	computer.offline = false
	computer.replaced = nil
	if _, err := computer.install(0); err != nil {
		t.Fatal(err)
	}
	if got := computer.recordList(); got != "machine_4" {
		t.Fatalf("after the working pairing the account holds %s, want only machine_4", got)
	}
	if got := strings.Join(computer.replaced, ","); got != "machine_4<machine_3,machine_4<machine_2,machine_4<machine_old" {
		t.Fatalf("replace calls %s, want newest first and the working record last", got)
	}
}
