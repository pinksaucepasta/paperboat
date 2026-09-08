//go:build darwin

package workerupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallDarwinPackageExtractsCanonicalPayload(t *testing.T) {
	root := newDarwinPackageFixture(t)
	withPkgutilStub(t, func(_ context.Context, arguments ...string) ([]byte, error) {
		switch arguments[0] {
		case "--payload-files":
			return []byte(".\n./usr\n./usr/local\n./usr/local/bin\n" + darwinPackageCLIPath + "\n./Library\n./Library/PrivilegedHelperTools\n./Library/PrivilegedHelperTools/Paperboat\n" + darwinPackageHelperPath + "\n"), nil
		case "--expand-full":
			writeDarwinExpandedPackage(t, arguments[2], darwinPackageIdentifier, "")
			return nil, nil
		default:
			return nil, errors.New("unexpected pkgutil operation")
		}
	})

	got, err := ExtractDarwinPackage(context.Background(), root.packagePath, root.extractionRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root.extractionRoot, "expanded", "Payload", "Library/PrivilegedHelperTools/Paperboat/bin/pb")
	if got != want {
		t.Fatalf("extracted executable = %q, want %q", got, want)
	}
	if info, err := os.Stat(got); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("canonical payload = %v, err=%v", info, err)
	}
}

func TestInstallDarwinPackageRejectsUnexpectedIdentifier(t *testing.T) {
	root := newDarwinPackageFixture(t)
	withPkgutilStub(t, func(_ context.Context, arguments ...string) ([]byte, error) {
		if arguments[0] == "--payload-files" {
			return []byte(darwinPackageCLIPath + "\n" + darwinPackageHelperPath + "\n"), nil
		}
		writeDarwinExpandedPackage(t, arguments[2], "dev.pprbt.other", "")
		return nil, nil
	})

	if _, err := ExtractDarwinPackage(context.Background(), root.packagePath, root.extractionRoot); !errors.Is(err, errPackageInstall) {
		t.Fatalf("error = %v, want package-install failure", err)
	}
}

func TestInstallDarwinPackageRejectsSymlinkPayload(t *testing.T) {
	root := newDarwinPackageFixture(t)
	withPkgutilStub(t, func(_ context.Context, arguments ...string) ([]byte, error) {
		if arguments[0] == "--payload-files" {
			return []byte(darwinPackageCLIPath + "\n" + darwinPackageHelperPath + "\n"), nil
		}
		writeDarwinExpandedPackage(t, arguments[2], darwinPackageIdentifier, "/tmp/outside-pb")
		return nil, nil
	})

	if _, err := ExtractDarwinPackage(context.Background(), root.packagePath, root.extractionRoot); !errors.Is(err, errPackageInstall) {
		t.Fatalf("error = %v, want package-install failure", err)
	}
}

func TestInstallDarwinPackageRejectsUnexpectedPayloadPath(t *testing.T) {
	root := newDarwinPackageFixture(t)
	withPkgutilStub(t, func(_ context.Context, arguments ...string) ([]byte, error) {
		if arguments[0] == "--payload-files" {
			return []byte(darwinPackageCLIPath + "\n" + darwinPackageHelperPath + "\n./unexpected\n"), nil
		}
		t.Fatal("expanded an invalid payload inventory")
		return nil, nil
	})
	if _, err := ExtractDarwinPackage(context.Background(), root.packagePath, root.extractionRoot); !errors.Is(err, errPackageInstall) {
		t.Fatalf("error = %v, want package-install failure", err)
	}
}

func TestInstallDarwinPackageRejectsUnexpectedExpandedPath(t *testing.T) {
	root := newDarwinPackageFixture(t)
	withPkgutilStub(t, func(_ context.Context, arguments ...string) ([]byte, error) {
		if arguments[0] == "--payload-files" {
			return []byte(darwinPackageCLIPath + "\n" + darwinPackageHelperPath + "\n"), nil
		}
		writeDarwinExpandedPackage(t, arguments[2], darwinPackageIdentifier, "")
		if err := os.WriteFile(filepath.Join(arguments[2], "Payload/unexpected"), []byte("unexpected"), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil, nil
	})
	if _, err := ExtractDarwinPackage(context.Background(), root.packagePath, root.extractionRoot); !errors.Is(err, errPackageInstall) {
		t.Fatalf("error = %v, want package-install failure", err)
	}
}

type darwinPackageFixture struct {
	packagePath    string
	extractionRoot string
}

func newDarwinPackageFixture(t *testing.T) darwinPackageFixture {
	t.Helper()
	root := t.TempDir()
	packagePath := filepath.Join(root, "paperboat.pkg")
	if err := os.WriteFile(packagePath, []byte("verified package"), 0o600); err != nil {
		t.Fatal(err)
	}
	extractionRoot := filepath.Join(root, "extraction")
	if err := os.Mkdir(extractionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return darwinPackageFixture{packagePath: packagePath, extractionRoot: extractionRoot}
}

func withPkgutilStub(t *testing.T, stub func(context.Context, ...string) ([]byte, error)) {
	t.Helper()
	previous := runPkgutil
	runPkgutil = stub
	t.Cleanup(func() { runPkgutil = previous })
}

func writeDarwinExpandedPackage(t *testing.T, root, identifier, cliTarget string) {
	t.Helper()
	payloadRoot := filepath.Join(root, "Payload")
	if err := os.MkdirAll(filepath.Join(payloadRoot, "usr/local/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(payloadRoot, "Library/PrivilegedHelperTools/Paperboat/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	packageInfo := `<pkg-info format-version="2" identifier="` + identifier + `" version="2026.09.07.1" install-location="/" />`
	if err := os.WriteFile(filepath.Join(root, "PackageInfo"), []byte(packageInfo), 0o600); err != nil {
		t.Fatal(err)
	}
	if cliTarget == "" {
		cliTarget = darwinPackageCLITarget
	}
	if err := os.Symlink(cliTarget, filepath.Join(payloadRoot, "usr/local/bin/pb")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payloadRoot, "Library/PrivilegedHelperTools/Paperboat/bin/pb"), []byte("signed pb"), 0o755); err != nil {
		t.Fatal(err)
	}
}
