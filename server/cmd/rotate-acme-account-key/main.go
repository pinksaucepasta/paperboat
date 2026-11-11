// rotate-acme-account-key rolls the ACME account key over (RFC 8555 section 7.3.5) and rewrites it in server.env.
// The account and its URL stay; the old key stops working at once. Run it on the host that holds server.env.
package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

const (
	directoryVariable = "PAPERBOAT_CERTIFICATES_DIRECTORY_URL"
	kidVariable       = "PAPERBOAT_CERTIFICATES_ACCOUNT_KID"
	referenceVariable = "PAPERBOAT_CERTIFICATES_ACCOUNT_KEY_REFERENCE"
	userAgent         = "paperboat-rotate-acme-account-key/1"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "rotate-acme-account-key:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("rotate-acme-account-key", flag.ContinueOnError)
	envPath := flags.String("env", "", "path of server.env (required)")
	directory := flags.String("directory-url", "", "ACME directory URL (default: "+directoryVariable+" from the env file)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *envPath == "" {
		return errors.New("-env is required")
	}
	text, err := os.ReadFile(*envPath)
	if err != nil {
		return fmt.Errorf("read env file: %w", err)
	}
	entries, err := parseEnv(string(text))
	if err != nil {
		return err
	}
	settings, err := readSettings(entries, *directory)
	if err != nil {
		return err
	}
	oldKey, err := tunnelcert.EnvironmentSignerSource{EnvironmentReferenceSource: tunnelcert.EnvironmentReferenceSource{
		LookupEnv: func(name string) (string, bool) {
			if e, err := lookup(entries, name); err == nil {
				return e.value, true
			}
			return "", false
		},
	}}.ResolveSigner(ctx, settings.reference)
	if err != nil {
		return fmt.Errorf("read the account key for %s from the env file: %w", settings.reference, err)
	}
	oldEntry, err := lookup(entries, settings.keyVariable)
	if err != nil {
		return err
	}

	client := &acme.Client{Key: oldKey, KID: acme.KeyID(settings.kid), DirectoryURL: settings.directory, HTTPClient: &http.Client{Timeout: 30 * time.Second}, UserAgent: userAgent}
	account, err := client.GetReg(ctx, "")
	if err != nil {
		return fmt.Errorf("the CA does not accept the current account key (already rolled over?): %w", err)
	}
	accountURL := account.URI
	if settings.kid != "" && accountURL != settings.kid {
		return errors.New("the CA reports a different account URL than the env file")
	}

	newKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate the new account key: %w", err)
	}
	newPEM, err := encodeKey(newKey)
	if err != nil {
		return err
	}
	oldPEM, err := encodeKey(oldKey)
	if err != nil {
		return err
	}
	// Everything that can fail is done before the CA changes: the old key is backed up and the new env file is
	// complete on disk, so the only step left after the rollover is a rename.
	backup, err := writeNew(*envPath+".old-acme-key."+time.Now().UTC().Format("20060102T150405Z"), oldPEM, *envPath, 0o600)
	if err != nil {
		return fmt.Errorf("write the backup of the old key: %w", err)
	}
	updated, err := replaceValue(string(text), oldEntry, strings.TrimSpace(newPEM))
	if err != nil {
		return err
	}
	staged, err := writeNew(*envPath+".new", updated, *envPath, 0)
	if err != nil {
		return fmt.Errorf("stage the new env file: %w", err)
	}
	if err := client.AccountKeyRollover(ctx, newKey); err != nil {
		os.Remove(staged)
		os.Remove(backup)
		return fmt.Errorf("key rollover refused, nothing changed: %w", err)
	}
	if err := os.Rename(staged, *envPath); err != nil {
		return fmt.Errorf("the CA now only accepts the new key, but replacing the env file failed (the complete new file is %s; move it over %s): %w", staged, *envPath, err)
	}
	if err := syncDir(filepath.Dir(*envPath)); err != nil {
		return fmt.Errorf("env file replaced but the directory sync failed: %w", err)
	}
	if err := verify(ctx, settings, accountURL, oldKey, newKey); err != nil {
		return fmt.Errorf("server.env is updated (backup %s) but verification failed: %w", backup, err)
	}
	parsed, _ := url.Parse(accountURL)
	fmt.Fprintf(out, "rolled over account %s%s; server.env updated; backup %s\n", parsed.Host, parsed.Path, backup)
	return nil
}

type settings struct {
	directory, kid, reference, keyVariable string
}

func readSettings(entries []entry, directoryFlag string) (settings, error) {
	var s settings
	s.directory = directoryFlag
	if s.directory == "" {
		e, err := lookup(entries, directoryVariable)
		if err != nil {
			return s, fmt.Errorf("%w (or pass -directory-url)", err)
		}
		s.directory = e.value
	}
	if e, err := lookup(entries, kidVariable); err == nil {
		s.kid = e.value
	}
	reference, err := lookup(entries, referenceVariable)
	if err != nil {
		return s, err
	}
	s.reference = reference.value
	if s.keyVariable, err = tunnelcert.EnvironmentReferenceName("", s.reference); err != nil {
		return s, fmt.Errorf("account key reference is invalid: %w", err)
	}
	return s, nil
}

// verify proves the CA answers to the new key and refuses the old one.
func verify(ctx context.Context, s settings, accountURL string, oldKey, newKey crypto.Signer) error {
	probe := func(key crypto.Signer) error {
		c := &acme.Client{Key: key, KID: acme.KeyID(accountURL), DirectoryURL: s.directory, HTTPClient: &http.Client{Timeout: 30 * time.Second}, UserAgent: userAgent}
		account, err := c.GetReg(ctx, "")
		if err == nil && account.URI != accountURL {
			return errors.New("the key belongs to a different account")
		}
		return err
	}
	if err := probe(newKey); err != nil {
		return fmt.Errorf("the CA does not accept the new key: %w", err)
	}
	err := probe(oldKey)
	var problem *acme.Error
	if err == nil {
		return errors.New("the CA still accepts the old key")
	}
	// the library reports an unknown key as ErrNoAccount, other CA refusals as *acme.Error
	if !errors.Is(err, acme.ErrNoAccount) && !errors.As(err, &problem) {
		return fmt.Errorf("could not tell whether the old key is refused: %w", err)
	}
	return nil
}

func encodeKey(key crypto.Signer) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("encode key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// writeNew creates path exclusively with the owner of reference and the given mode (0: the mode of reference),
// writes content and syncs it.
func writeNew(path, content, reference string, mode os.FileMode) (string, error) {
	info, err := os.Stat(reference)
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		file.Close()
		os.Remove(path)
		return "", err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := file.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
			return fail(err)
		}
	}
	if mode == 0 {
		mode = info.Mode().Perm()
	}
	if err := file.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := file.WriteString(content); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	return path, file.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
