package identity

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newControlCauseStore(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "identity")
	store, err := Open(Config{StateRoot: root, Random: bytes.NewReader(bytes.Repeat([]byte{0x2a}, 32))})
	if err != nil {
		t.Fatal("open identity store")
	}
	return store, root, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
}

func saveControlCauseRegistration(t *testing.T, store *Store, root string, now time.Time) Registration {
	t.Helper()
	key := store.Current()
	registration := Registration{
		ServerURL:              "https://api.example.test",
		MachineID:              "machine_1",
		EnvironmentID:          "environment_1",
		PublicKeyID:            key.ID,
		PublicIdentityKey:      base64.RawURLEncoding.EncodeToString(key.Public()),
		InboxPath:              filepath.Join(root, "inbox"),
		InstallationGeneration: 3,
		UpdatedAt:              now,
	}
	if err := store.SaveRegistration(registration); err != nil {
		t.Fatal("save registration")
	}
	return registration
}

func requireControlCauseAndStaticMessage(t *testing.T, err error, path, marker string) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrInvalidStore) {
		t.Fatalf("error=%v, want invalid-store classification", err)
	}
	if path != "" && strings.Contains(err.Error(), path) {
		t.Fatalf("public error disclosed path: %q", err.Error())
	}
	if marker != "" && strings.Contains(err.Error(), marker) {
		t.Fatalf("public error disclosed document content: %q", err.Error())
	}
}

func TestMachineControlPreservesFilesystemAndDecodeCausesPrivately(t *testing.T) {
	store, root, now := newControlCauseStore(t)
	controlPath := filepath.Join(root, "machine-control.json")

	if _, err := store.MachineControl(now, 0); err == nil {
		t.Fatal("missing control document was accepted")
	} else {
		requireControlCauseAndStaticMessage(t, err, root, "")
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing control cause lost: %T", err)
		}
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			t.Fatalf("missing control path cause type lost: %T", err)
		}
	}

	for name, contents := range map[string][]byte{
		"initial document":  []byte(`{"credential":"control-document-private-marker"`),
		"trailing document": []byte(`{"version":1} {"private_marker":`),
	} {
		if err := os.WriteFile(controlPath, contents, 0o600); err != nil {
			t.Fatal("write malformed control document")
		}
		_, err := store.MachineControl(now, 0)
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
		requireControlCauseAndStaticMessage(t, err, root, "control-document-private-marker")
		if strings.Contains(err.Error(), "private_marker") {
			t.Fatalf("%s error disclosed document structure: %q", name, err.Error())
		}
		var syntaxErr *json.SyntaxError
		if !errors.As(err, &syntaxErr) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("%s decoder cause type lost: %T", name, err)
		}
		if name == "trailing document" && !errors.Is(err, os.ErrNotExist) {
			t.Fatal("simultaneous registration failure was lost behind the trailing decoder error")
		}
	}
}

func TestMachineControlAndProofPreserveRegistrationCause(t *testing.T) {
	store, root, now := newControlCauseStore(t)
	registration := saveControlCauseRegistration(t, store, root, now)
	key := store.Current()
	control := MachineControl{
		MachineID:              registration.MachineID,
		EnvironmentID:          registration.EnvironmentID,
		InstallationGeneration: registration.InstallationGeneration,
		Credential:             strings.Repeat("c", 32),
		ExpiresAt:              now.Add(time.Hour),
		KeyID:                  key.ID,
	}
	if err := store.SaveMachineControl(control); err != nil {
		t.Fatal("save machine control")
	}
	registrationPath := filepath.Join(root, "machine-registration.json")
	if err := os.Remove(registrationPath); err != nil {
		t.Fatal("remove registration fixture")
	}

	checkRegistrationCause := func(name string, err error) {
		t.Helper()
		requireControlCauseAndStaticMessage(t, err, root, "")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s lost registration not-exist cause: %T", name, err)
		}
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			t.Errorf("%s lost registration path cause type: %T", name, err)
		}
	}

	if _, err := store.MachineControl(now, 0); err == nil {
		t.Error("machine control accepted missing registration")
	} else {
		checkRegistrationCause("MachineControl", err)
	}
	if err := store.SaveMachineControl(control); err == nil {
		t.Error("machine control save accepted missing registration")
	} else {
		checkRegistrationCause("SaveMachineControl", err)
	}
	if _, err := store.MachineProof("operation-1", http.MethodPost, "/v1/machine-control-renewals", nil, now); err == nil {
		t.Error("machine proof accepted missing registration")
	} else {
		checkRegistrationCause("MachineProof", err)
	}
}
