//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func accountReadableCarrierFixture(root, account, group string, readable bool) (*Installer, error) {
	return New(Config{Platform: runtime.GOOS, Kind: CarrierKind, Instance: "u65534", ConfigRoot: root, Executable: filepath.Join(root, "pb"), User: account, Group: group, Arguments: []string{"daemon", "__runtime-carrier"}, AccountReadableDefinition: readable, Controller: SystemdController{Runner: ExecRunner{}}})
}

func TestCarrierDeclarationReadableWithoutPublicationAuthority(t *testing.T) {
	if root := os.Getenv("PB_TEST_CARRIER_DEFINITION_ROOT"); root != "" {
		i, err := accountReadableCarrierFixture(root, os.Getenv("PB_TEST_CARRIER_USER"), os.Getenv("PB_TEST_CARRIER_GROUP"), true)
		if err != nil {
			t.Fatal(err)
		}
		if err = i.VerifyPublishedDefinition(); err != nil {
			t.Fatal("enrolled account cannot verify protected native declaration")
		}
		if err = os.WriteFile(i.DefinitionPath(), []byte("unauthorized replacement"), 0600); !errors.Is(err, syscall.EACCES) {
			t.Fatal("enrolled account obtained native publication authority")
		}
		if err = os.Chmod(i.DefinitionPath(), 0666); !errors.Is(err, os.ErrPermission) {
			t.Fatal("enrolled account can grant itself publication authority")
		}
		if err = os.Remove(i.DefinitionPath()); !errors.Is(err, os.ErrPermission) {
			t.Fatal("enrolled account can replace native declaration inode")
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires different-account native declaration permission check")
	}
	account, err := user.LookupId("65534")
	if err != nil {
		t.Skip("isolated unprivileged account unavailable")
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "pb-native-definition-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err = os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(root, "pb")
	if err = os.WriteFile(pin, image, 0755); err != nil {
		t.Fatal(err)
	}
	i, err := accountReadableCarrierFixture(root, account.Username, group.Name, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = i.PublishDefinition(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, pin, "-test.run=^TestCarrierDeclarationReadableWithoutPublicationAuthority$")
	child.Env = append(os.Environ(), "PB_TEST_CARRIER_DEFINITION_ROOT="+root, "PB_TEST_CARRIER_USER="+account.Username, "PB_TEST_CARRIER_GROUP="+group.Name)
	child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: uint32(gid)}}
	if err = child.Run(); err != nil {
		t.Fatal("different-account native verification or write denial failed", err)
	}
	if err = i.VerifyPublishedDefinition(); err != nil {
		t.Fatal("native declaration changed after denied write", err)
	}
	private, err := New(Config{Platform: runtime.GOOS, Kind: UpdaterKind, Instance: "u65534", ConfigRoot: root, Executable: pin, User: account.Username, Group: group.Name, Arguments: []string{"daemon", "__runtime-updated"}, Controller: SystemdController{Runner: ExecRunner{}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = private.PublishDefinition(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(private.DefinitionPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unrelated native declaration readability changed", err)
	}
	restricted := private.config
	restricted.AccountReadableDefinition = true
	if _, err = New(restricted); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatal("unrelated role gained account-readable policy")
	}
}
