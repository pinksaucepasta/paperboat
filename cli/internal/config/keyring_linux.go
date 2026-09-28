//go:build linux

package config

import (
	"crypto/subtle"
	"errors"
	"fmt"
	dbus "github.com/godbus/dbus/v5"
	secretservice "github.com/zalando/go-keyring/secret_service"
	"os"
	"sort"
)

type KeyringStore struct{}

var errKeyringSecretNotFound = errors.New("credential not found")
var errKeyringSecretAmbiguous = errors.New("duplicate credentials found")

func unavailableCredentialStore(err error) error {
	return fmt.Errorf("%w: %v", ErrCredentialStoreUnavailable, err)
}

func linuxSecretAttributes(ref string) map[string]string {
	return map[string]string{"service": keyringService, "account": ref}
}

// CredentialStoreAvailable reports whether the current login session has a
// usable persistent Secret Service collection. Headless Linux sessions may
// have a D-Bus owner but no login or default collection; in that case callers
// use the protected owner-only file store.
func CredentialStoreAvailable() bool {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return false
	}
	bus, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	defer bus.Close()
	var owned bool
	if err := bus.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.freedesktop.secrets").Store(&owned); err != nil {
		return false
	}
	if !owned {
		return false
	}
	service := bus.Object("org.freedesktop.secrets", "/org/freedesktop/secrets")
	collections, err := service.GetProperty("org.freedesktop.Secret.Service.Collections")
	if err != nil {
		return false
	}
	paths, ok := collections.Value().([]dbus.ObjectPath)
	if !ok {
		return false
	}
	for _, path := range paths {
		if path == "/org/freedesktop/secrets/collection/login" {
			return true
		}
	}
	var defaultCollection dbus.ObjectPath
	if err := service.Call("org.freedesktop.Secret.Service.ReadAlias", 0, "default").Store(&defaultCollection); err != nil {
		return false
	}
	return defaultCollection.IsValid() && defaultCollection != "/"
}
func linuxSecretItems(service *secretservice.SecretService, ref string) ([]dbus.ObjectPath, error) {
	collection := service.GetLoginCollection()
	if err := service.Unlock(collection.Path()); err != nil {
		return nil, err
	}
	items, err := service.SearchItems(collection, linuxSecretAttributes(ref))
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, errKeyringSecretNotFound
	}
	sort.Slice(items, func(left, right int) bool { return string(items[left]) < string(items[right]) })
	return items, nil
}

func linuxSecretItem(service *secretservice.SecretService, ref string) (dbus.ObjectPath, error) {
	items, err := linuxSecretItems(service, ref)
	if err != nil {
		return "", err
	}
	if len(items) != 1 {
		return "", errKeyringSecretAmbiguous
	}
	return items[0], nil
}
func (KeyringStore) Set(ref, value string) error {
	service, err := secretservice.NewSecretService()
	if err != nil {
		return unavailableCredentialStore(err)
	}
	session, err := service.OpenSession()
	if err != nil {
		return unavailableCredentialStore(err)
	}
	defer service.Close(session)
	collection := service.GetLoginCollection()
	if err := service.Unlock(collection.Path()); err != nil {
		return unavailableCredentialStore(err)
	}
	if err := service.CreateItem(collection, fmt.Sprintf("%s/%s", keyringService, ref), linuxSecretAttributes(ref), secretservice.NewSecret(session.Path(), value)); err != nil {
		return unavailableCredentialStore(err)
	}
	// Secret Service's replace flag is not consistently implemented when a
	// legacy client already created duplicate matching items. Reconcile to one
	// deterministic item before reporting success so reads can always fail
	// closed instead of selecting an arbitrary old secret.
	items, err := linuxSecretItems(service, ref)
	if err != nil {
		return unavailableCredentialStore(err)
	}
	desired := []byte(value)
	defer clear(desired)
	var keep dbus.ObjectPath
	for _, item := range items {
		if err := service.Unlock(item); err != nil {
			return unavailableCredentialStore(err)
		}
		secret, err := service.GetSecret(item, session.Path())
		if err != nil {
			return unavailableCredentialStore(err)
		}
		matches := subtle.ConstantTimeCompare(secret.Value, desired) == 1
		clear(secret.Value)
		if matches && keep == "" {
			keep = item
		}
	}
	if keep == "" {
		return unavailableCredentialStore(errKeyringSecretAmbiguous)
	}
	for _, item := range items {
		if item != keep {
			if err := service.Delete(item); err != nil {
				return unavailableCredentialStore(err)
			}
		}
	}
	canonical, err := linuxSecretItems(service, ref)
	if err != nil || len(canonical) != 1 || canonical[0] != keep {
		if err == nil {
			err = errKeyringSecretAmbiguous
		}
		return unavailableCredentialStore(err)
	}
	return nil
}
func (KeyringStore) Get(ref string) (string, error) {
	service, err := secretservice.NewSecretService()
	if err != nil {
		return "", unavailableCredentialStore(err)
	}
	item, err := linuxSecretItem(service, ref)
	if errors.Is(err, errKeyringSecretNotFound) {
		return "", ErrSecretNotFound
	}
	if err != nil {
		return "", unavailableCredentialStore(err)
	}
	session, err := service.OpenSession()
	if err != nil {
		return "", unavailableCredentialStore(err)
	}
	defer service.Close(session)
	if err := service.Unlock(item); err != nil {
		return "", unavailableCredentialStore(err)
	}
	secret, err := service.GetSecret(item, session.Path())
	if err != nil {
		return "", unavailableCredentialStore(err)
	}
	return string(secret.Value), nil
}
func (KeyringStore) Delete(ref string) error {
	service, err := secretservice.NewSecretService()
	if err != nil {
		return unavailableCredentialStore(err)
	}
	items, err := linuxSecretItems(service, ref)
	if errors.Is(err, errKeyringSecretNotFound) {
		return nil
	}
	if err != nil {
		return unavailableCredentialStore(err)
	}
	for _, item := range items {
		if err := service.Delete(item); err != nil {
			return unavailableCredentialStore(err)
		}
	}
	return nil
}
