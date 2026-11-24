//go:build windows

package machineguard

import (
	"crypto/x509"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	crypt32                 = windows.NewLazySystemDLL("crypt32.dll")
	certAddEncodedCRL       = crypt32.NewProc("CertAddEncodedCRLToStore")
	certEnumCRLs            = crypt32.NewProc("CertEnumCRLsInStore")
	certDeleteCRL           = crypt32.NewProc("CertDeleteCRLFromStore")
	certDuplicateCRL        = crypt32.NewProc("CertDuplicateCRLContext")
	errNoMoreCRLs     error = windows.Errno(windows.CRYPT_E_NOT_FOUND)
)

// crlContext is wincrypt.h CRL_CONTEXT.
type crlContext struct {
	encoding uint32
	encoded  *byte
	length   uint32
	info     uintptr
	store    windows.Handle
}

// publishWindowsCRL puts the list where Windows looks before its distribution point: that URL is on port 80, which
// HTTP.sys often holds, and Schannel clients such as curl.exe then fail with CRYPT_E_REVOCATION_OFFLINE.
func publishWindowsCRL(der []byte) error {
	store, err := openWindowsRootStore(windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, "CA")
	if err != nil {
		return err
	}
	defer windows.CertCloseStore(store, 0)
	return addCRLToStore(store, der)
}

func addCRLToStore(store windows.Handle, der []byte) error {
	if len(der) == 0 {
		return errors.New("local revocation list is empty")
	}
	ok, _, err := certAddEncodedCRL.Call(uintptr(store), windows.X509_ASN_ENCODING, uintptr(unsafe.Pointer(&der[0])), uintptr(len(der)), windows.CERT_STORE_ADD_REPLACE_EXISTING, 0)
	if ok == 0 {
		return fmt.Errorf("add Paperboat revocation list to LocalMachine: %w", err)
	}
	return nil
}

func withdrawWindowsCRL(root *x509.Certificate) error {
	store, err := openWindowsRootStore(windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, "CA")
	if err != nil {
		return err
	}
	defer windows.CertCloseStore(store, 0)
	return removeCRLsFromStore(store, root)
}

// removeCRLsFromStore removes only the lists root signed.
func removeCRLsFromStore(store windows.Handle, root *x509.Certificate) error {
	var previous uintptr
	for {
		current, _, err := certEnumCRLs.Call(uintptr(store), previous)
		if current == 0 {
			if errors.Is(err, errNoMoreCRLs) {
				return nil
			}
			return fmt.Errorf("read LocalMachine revocation lists: %w", err)
		}
		previous = current
		crl := *(**crlContext)(unsafe.Pointer(&current))
		list, err := x509.ParseRevocationList(unsafe.Slice(crl.encoded, crl.length))
		if err != nil || list.CheckSignatureFrom(root) != nil {
			continue
		}
		// Delete frees the context it is given, so the enumeration keeps its own reference.
		duplicate, _, _ := certDuplicateCRL.Call(current)
		if ok, _, err := certDeleteCRL.Call(duplicate); ok == 0 {
			return fmt.Errorf("remove Paperboat revocation list: %w", err)
		}
	}
}

var platformPublishCRL = publishWindowsCRL
