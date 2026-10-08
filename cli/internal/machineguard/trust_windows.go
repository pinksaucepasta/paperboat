//go:build windows

package machineguard

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func installLocalTrust(_ context.Context, _ Config, certificatePEM []byte) error {
	certificate, err := parseLocalTrustRoot(certificatePEM)
	if err != nil {
		return err
	}
	return addWindowsRootToStore(certificate, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, "ROOT")
}

func removeLocalTrust(ctx context.Context, _ Config, certificatePEM []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	certificate, err := parseLocalTrustRoot(certificatePEM)
	if err != nil {
		return err
	}
	return removeWindowsRootFromStore(certificate, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, "ROOT")
}

func cleanupHistoricalTrustExcept(ctx context.Context, cfg Config, activePEM []byte) error {
	retired, err := historicalRootsExcept(cfg.StateDir, activePEM)
	if err != nil || len(retired) == 0 {
		return err
	}
	return cleanupWindowsOwnedTrust(ctx, retired)
}

func InstallUserTrust(context.Context, []byte) error { return nil }
func RemoveUserTrust(context.Context, []byte) error  { return nil }
func CleanupUserTrust(context.Context) error         { return nil }

func addWindowsRootToStore(certificate *x509.Certificate, storeLocation uintptr, storeName string) error {
	store, err := openWindowsRootStore(storeLocation, storeName)
	if err != nil {
		return err
	}
	defer windows.CertCloseStore(store, 0)
	if found, err := windowsRootPresent(store, certificate.Raw); err != nil || found {
		return err
	}
	created, err := windows.CertCreateCertificateContext(windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, &certificate.Raw[0], uint32(len(certificate.Raw)))
	if err != nil {
		return err
	}
	defer windows.CertFreeCertificateContext(created)
	var added *windows.CertContext
	if err = windows.CertAddCertificateContextToStore(store, created, windows.CERT_STORE_ADD_NEW, &added); err != nil {
		// Another startup may have inserted the same root between enumeration and
		// insertion. Confirm exact bytes before treating that as idempotent.
		if found, checkErr := windowsRootPresent(store, certificate.Raw); checkErr == nil && found {
			return nil
		}
		return fmt.Errorf("add Paperboat root to LocalMachine trust: %w", err)
	}
	if added != nil {
		windows.CertFreeCertificateContext(added)
	}
	return nil
}

func removeWindowsRootFromStore(certificate *x509.Certificate, storeLocation uintptr, storeName string) error {
	store, err := openWindowsRootStore(storeLocation, storeName)
	if err != nil {
		return err
	}
	defer windows.CertCloseStore(store, 0)
	var previous *windows.CertContext
	for {
		current, enumErr := windows.CertEnumCertificatesInStore(store, previous)
		if enumErr != nil {
			if errors.Is(enumErr, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return nil
			}
			return enumErr
		}
		if current == nil {
			return nil
		}
		previous = current
		if bytes.Equal(unsafe.Slice(current.EncodedCert, current.Length), certificate.Raw) {
			if err = windows.CertDeleteCertificateFromStore(current); err != nil {
				return err
			}
			previous = nil
		}
	}
}

func windowsRootPresent(store windows.Handle, encoded []byte) (bool, error) {
	var previous *windows.CertContext
	for {
		current, err := windows.CertEnumCertificatesInStore(store, previous)
		if err != nil {
			if errors.Is(err, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return false, nil
			}
			return false, err
		}
		if current == nil {
			return false, nil
		}
		previous = current
		if bytes.Equal(unsafe.Slice(current.EncodedCert, current.Length), encoded) {
			_ = windows.CertFreeCertificateContext(current)
			return true, nil
		}
	}
}

func openWindowsRootStore(storeLocation uintptr, storeName string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(storeName)
	if err != nil {
		return 0, err
	}
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM_W, 0, 0, uint32(storeLocation)|windows.CERT_STORE_OPEN_EXISTING_FLAG, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return 0, err
	}
	return store, nil
}
